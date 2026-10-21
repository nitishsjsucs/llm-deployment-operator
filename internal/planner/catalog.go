/*
Copyright 2026 The llm-deployment-operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planner

import (
	"fmt"
	"sort"
)

// Architecture holds the model dimensions that drive memory use.
type Architecture struct {
	// Family is "llama" or "qwen".
	Family string
	// Params is the total parameter count.
	Params int64
	// Layers is the number of transformer blocks (num_hidden_layers).
	Layers int64
	// KVHeads is the number of key/value heads (num_key_value_heads).
	KVHeads int64
	// HeadDim is the per-head dimension.
	HeadDim int64
	// HiddenSize is the residual stream width (hidden_size).
	HiddenSize int64
	// VocabSize is the vocabulary size (vocab_size).
	VocabSize int64
	// TiedEmbeddings is true when lm_head shares the input embedding matrix.
	TiedEmbeddings bool
	// MaxContextLen is the native context window (max_position_embeddings).
	MaxContextLen int64
}

// EmbeddingParams is the parameter count of the input embedding plus the LM
// head. AWQ and GPTQ checkpoints keep these tensors in 16-bit precision.
func (a Architecture) EmbeddingParams() int64 {
	n := a.VocabSize * a.HiddenSize
	if !a.TiedEmbeddings {
		n *= 2
	}
	return n
}

// Validate reports dimensions that cannot describe a real model.
func (a Architecture) Validate() error {
	switch {
	case a.Params <= 0, a.Layers <= 0, a.KVHeads <= 0, a.HeadDim <= 0, a.HiddenSize <= 0, a.VocabSize <= 0:
		return fmt.Errorf("architecture dimensions must all be positive")
	case a.EmbeddingParams() >= a.Params:
		return fmt.Errorf("embedding parameters (%d) must be fewer than total parameters (%d)", a.EmbeddingParams(), a.Params)
	}
	return nil
}

// catalog values are taken from each model's Hugging Face config.json and
// safetensors parameter count.
var catalog = map[string]Architecture{
	"llama-3.1-8b": {
		Family: "llama", Params: 8_030_261_248, Layers: 32, KVHeads: 8, HeadDim: 128,
		HiddenSize: 4096, VocabSize: 128_256, MaxContextLen: 131_072,
	},
	"llama-3.1-70b": {
		Family: "llama", Params: 70_553_706_496, Layers: 80, KVHeads: 8, HeadDim: 128,
		HiddenSize: 8192, VocabSize: 128_256, MaxContextLen: 131_072,
	},
	"qwen2.5-7b": {
		Family: "qwen", Params: 7_615_616_512, Layers: 28, KVHeads: 4, HeadDim: 128,
		HiddenSize: 3584, VocabSize: 152_064, MaxContextLen: 32_768,
	},
	"qwen2.5-14b": {
		Family: "qwen", Params: 14_770_033_664, Layers: 48, KVHeads: 8, HeadDim: 128,
		HiddenSize: 5120, VocabSize: 152_064, MaxContextLen: 32_768,
	},
	"qwen2.5-72b": {
		Family: "qwen", Params: 72_706_203_648, Layers: 80, KVHeads: 8, HeadDim: 128,
		HiddenSize: 8192, VocabSize: 152_064, MaxContextLen: 32_768,
	},
}

// Lookup returns the catalog entry with the given ID, e.g. "llama-3.1-8b".
func Lookup(id string) (Architecture, bool) {
	a, ok := catalog[id]
	return a, ok
}

// CatalogIDs returns the catalog entry IDs in sorted order.
func CatalogIDs() []string {
	ids := make([]string, 0, len(catalog))
	for id := range catalog {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
