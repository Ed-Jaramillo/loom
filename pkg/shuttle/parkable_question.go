// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shuttle

// ParkableQuestion is a tool whose result IS a human's answer. With HITL park
// enabled the agent never executes such a call: the pre-scan parks the turn on
// it as a "question" item (exactly like contact_human), and on resume the
// agent builds the call's result from the human's answer via ParkAnswerResult.
// Execute still runs when the call is not parked (park disabled, or
// ParkQuestion reported ok=false) and should then explain why.
type ParkableQuestion interface {
	Tool
	// ParkQuestion reports whether this call should park and the human-facing
	// question text. ok=false leaves the call to normal dispatch (Execute),
	// e.g. so invalid input reaches the model as a repairable tool error.
	ParkQuestion(input map[string]interface{}) (question string, ok bool)
	// ParkAnswerResult builds the tool result from the human's non-empty
	// answer. It must not return nil.
	ParkAnswerResult(input map[string]interface{}, answer string) *Result
}
