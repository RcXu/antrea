// Copyright 2022 Antrea Authors
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

package types

const (
	NPLAnnotationKey        = "nodeportlocal.antrea.io"
	NPLEnabledAnnotationKey = "nodeportlocal.antrea.io/enabled"
)

// NPLAnnotation is the structure used for setting NodePortLocal annotation on the Pods.
type NPLAnnotation struct {
	PodPort   int      `json:"podPort"`
	NodeIP    string   `json:"nodeIP"`
	NodePort  int      `json:"nodePort"`
	Protocol  string   `json:"protocol"`
	Protocols []string `json:"protocols"` // deprecated, array with a single member which is equal to the Protocol field
}
