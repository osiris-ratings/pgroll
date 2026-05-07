// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizePreserveVersions(t *testing.T) {
	tests := []struct {
		name             string
		schema           string
		preserveVersions []string
		want             []string
	}{
		{
			name:             "bare version names are passed through",
			schema:           "public",
			preserveVersions: []string{"20260429150953_add_queue_priority"},
			want:             []string{"20260429150953_add_queue_priority"},
		},
		{
			name:             "full version-schema names have the schema prefix stripped",
			schema:           "public",
			preserveVersions: []string{"public_20260429150953_add_queue_priority"},
			want:             []string{"20260429150953_add_queue_priority"},
		},
		{
			name:             "mixed forms in a single call",
			schema:           "public",
			preserveVersions: []string{"public_20260429150953_a", "20260506133108_b"},
			want:             []string{"20260429150953_a", "20260506133108_b"},
		},
		{
			name:             "non-default schema is honored when stripping",
			schema:           "tenant",
			preserveVersions: []string{"tenant_v1_init"},
			want:             []string{"v1_init"},
		},
		{
			name:             "wrong-schema prefix is left untouched",
			schema:           "public",
			preserveVersions: []string{"tenant_v1_init"},
			want:             []string{"tenant_v1_init"},
		},
		{
			name:             "empty input yields empty slice",
			schema:           "public",
			preserveVersions: nil,
			want:             []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePreserveVersions(tc.schema, tc.preserveVersions)
			assert.Equal(t, tc.want, got)
		})
	}
}
