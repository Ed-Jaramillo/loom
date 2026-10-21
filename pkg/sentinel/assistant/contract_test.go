// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/teradata-labs/loom/pkg/prompts"
)

const findingsBlock = "```json\n" + `{
  "observed_facts": [
    {"statement": "llm.completion stopped at max_tokens", "span_ids": ["0000000000000001"]},
    {"statement": "made-up span", "span_ids": ["deadbeefdeadbeef"]},
    {"statement": "no citation", "span_ids": []}
  ],
  "candidate_explanations": [
    {"statement": "truncated call dispatched", "confidence": "HIGH", "supporting_span_ids": ["0000000000000002"], "contradicting_span_ids": ["cafebabecafebabe"]},
    {"statement": "guess", "confidence": "certain", "supporting_span_ids": []}
  ],
  "missing_information": ["whether the tool call was complete at dispatch"],
  "suggested_remedies": [{"description": "raise max_tokens", "kind": "config"}]
}` + "\n```"

func TestParseFindings_FencedBlock(t *testing.T) {
	text := "Here is what I found.\n\n```json\n{\"not\": \"findings\"}\n```\n\n" + findingsBlock + "\nDone."
	f := parseFindings(text)
	require.NotNil(t, f, "the last fenced block that decodes into findings wins")
	assert.Len(t, f.ObservedFacts, 3)
	require.Len(t, f.CandidateExplanations, 2)
	assert.Equal(t, "high", f.CandidateExplanations[0].Confidence)
	assert.Equal(t, "unspecified", f.CandidateExplanations[1].Confidence)
	assert.Equal(t, []string{"whether the tool call was complete at dispatch"}, f.MissingInformation)
	assert.Equal(t, "config", f.SuggestedRemedies[0].Kind)
}

func TestParseFindings_BareObjectAndNone(t *testing.T) {
	bare := `Summary first. {"observed_facts":[{"statement":"x","span_ids":["0000000000000001"]}],"missing_information":[]} trailing words`
	f := parseFindings(bare)
	require.NotNil(t, f)
	assert.Len(t, f.ObservedFacts, 1)

	assert.Nil(t, parseFindings("No structured block at all."))
	assert.Nil(t, parseFindings("```json\n{\"unrelated\": 1}\n```"))
	assert.Nil(t, parseFindings("```json\n{broken\n```"))
}

func TestLastJSONObject_SkipsNested(t *testing.T) {
	assert.Equal(t, `{"b":{"c":1}}`, lastJSONObject(`x {"a":1} y {"b":{"c":1}} z`))
	assert.Equal(t, "", lastJSONObject("no braces { here"))
}

func TestCheckCitations(t *testing.T) {
	trace := map[string]struct{}{
		"0000000000000001": {},
		"0000000000000002": {},
		"0000000000000003": {},
	}
	text := "Span 0000000000000003 shows the breaker. Trace d8f19f723c0b840e788cbfa0aed7c22c is not a span id.\n" + findingsBlock
	f := parseFindings(text)
	require.NotNil(t, f)
	check := checkCitations(text, f, trace, false)

	assert.True(t, check.Checked)
	assert.Equal(t, 3, check.SpansInTrace)
	assert.Equal(t, []string{"0000000000000001", "0000000000000002", "0000000000000003"}, check.VerifiedSpanIDs)
	assert.Equal(t, []string{"cafebabecafebabe", "deadbeefdeadbeef"}, check.UnverifiedSpanIDs,
		"the 32-hex trace id must not be read as two span ids")

	assert.True(t, f.ObservedFacts[0].Supported)
	assert.False(t, f.ObservedFacts[1].Supported)
	assert.Equal(t, []string{"deadbeefdeadbeef"}, f.ObservedFacts[1].UnverifiedSpanIDs)
	assert.False(t, f.ObservedFacts[2].Supported)
	assert.Equal(t, 2, check.UnsupportedFacts)

	assert.True(t, f.CandidateExplanations[0].Supported)
	assert.Equal(t, []string{"cafebabecafebabe"}, f.CandidateExplanations[0].UnverifiedSpanIDs)
	assert.False(t, f.CandidateExplanations[1].Supported)
	assert.Equal(t, 1, check.UnsupportedExplanations)
}

func TestCheckCitations_NoFindings(t *testing.T) {
	check := checkCitations("see span 00000000000000aa", nil, map[string]struct{}{}, true)
	assert.True(t, check.ListingTruncated)
	assert.Equal(t, []string{"00000000000000aa"}, check.UnverifiedSpanIDs)
}

func TestParseLoomContext(t *testing.T) {
	m, err := parseLoomContext(nil)
	require.NoError(t, err)
	assert.Empty(t, m)

	in := map[string]interface{}{"loom_run_id": "r1"}
	m, err = parseLoomContext(in)
	require.NoError(t, err)
	m["added"] = true
	_, mutated := in["added"]
	assert.False(t, mutated, "the caller's map is copied")

	m, err = parseLoomContext(`{"effective_config":{"max_tokens":{"value":4096,"source":"provider_default"}}}`)
	require.NoError(t, err)
	assert.Contains(t, m, "effective_config")

	_, err = parseLoomContext("not json")
	assert.Error(t, err)
	_, err = parseLoomContext(42)
	assert.Error(t, err)
}

func TestRenderRequest_DefaultInstructions(t *testing.T) {
	msg, err := renderRequest(context.Background(), nil, requestInput{
		Question:    "Why didn't the retry stop? Check \"shared_memory_write\".",
		LoomContext: map[string]interface{}{"loom_version": "1.4.0", "effective_config": map[string]interface{}{"max_tokens": 4096}},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(msg, "Why didn't the retry stop? Check \"shared_memory_write\".\n\n"),
		"the question is sent verbatim, not escaped")
	assert.Contains(t, msg, defaultRequestInstructions)
	idx := strings.Index(msg, loomContextLabel+"\n")
	require.GreaterOrEqual(t, idx, 0, "context label present")
	var sent map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(msg[idx+len(loomContextLabel)+1:]), &sent), "the context tail is valid JSON")
	assert.Equal(t, "1.4.0", sent["loom_version"])
	assert.NotContains(t, msg, "&#34;")
	assert.NotContains(t, strings.ToLower(msg), "you are ")
}

// stubRegistry implements prompts.PromptRegistry for RequestPromptKey only.
type stubRegistry struct {
	prompts.PromptRegistry
	content string
	err     error
	gotVars map[string]interface{}
	called  bool
}

func (s *stubRegistry) Get(_ context.Context, key string, vars map[string]interface{}) (string, error) {
	if key != RequestPromptKey {
		return "", errors.New("unexpected key " + key)
	}
	s.called = true
	s.gotVars = vars
	return s.content, s.err
}

func TestRenderRequest_RegistryOverrideAndFallback(t *testing.T) {
	reg := &stubRegistry{content: "  OVERRIDE INSTRUCTIONS  "}
	msg, err := renderRequest(context.Background(), reg, requestInput{Question: "q", LoomContext: map[string]interface{}{}})
	require.NoError(t, err)
	assert.True(t, reg.called)
	assert.Empty(t, reg.gotVars, "no values pass through the registry's escaping")
	assert.Equal(t, "q\n\nOVERRIDE INSTRUCTIONS\n\n"+loomContextLabel+"\n{}", msg)

	missing := &stubRegistry{err: errors.New("not found")}
	msg, err = renderRequest(context.Background(), missing, requestInput{Question: "q", LoomContext: map[string]interface{}{}})
	require.NoError(t, err)
	assert.Contains(t, msg, defaultRequestInstructions, "falls back to the built-in instructions")
}

// TestRequestPrompt_FileRegistry loads prompts/tools/ask_phoenix_assistant.yaml
// through the real FileRegistry: the key must resolve, and the file text must
// match the built-in fallback so the two cannot drift.
func TestRequestPrompt_FileRegistry(t *testing.T) {
	reg := prompts.NewFileRegistry("../../../prompts")
	require.NoError(t, reg.Reload(context.Background()))
	fromFile, err := reg.Get(context.Background(), RequestPromptKey, nil)
	require.NoError(t, err, "the prompt file must register under RequestPromptKey")
	assert.Equal(t, defaultRequestInstructions, strings.TrimSpace(fromFile))

	msg, err := renderRequest(context.Background(), reg, requestInput{
		Question:    "Why did the breaker trip?",
		LoomContext: map[string]interface{}{"loom_version": "1.4.0"},
	})
	require.NoError(t, err)
	assert.Contains(t, msg, `"loom_version": "1.4.0"`)
	assert.NotContains(t, msg, "&#34;")
}
