//go:build goexperiment.jsonv2

package runtime

import (
	"testing"
)

func TestExtractPath(t *testing.T) {
	tests := []struct {
		name    string
		data    []byte
		path    []string
		want    string
		wantErr bool
	}{
		{
			name: "top-level string",
			data: []byte(`{"name": "Alice"}`),
			path: []string{"name"},
			want: `"Alice"`,
		},
		{
			name: "top-level number",
			data: []byte(`{"amount": 42500.75}`),
			path: []string{"amount"},
			want: `42500.75`,
		},
		{
			name: "nested path",
			data: []byte(`{"user": {"name": "Bob", "age": 25}}`),
			path: []string{"user", "name"},
			want: `"Bob"`,
		},
		{
			name: "deeply nested",
			data: []byte(`{"a": {"b": {"c": 99}}}`),
			path: []string{"a", "b", "c"},
			want: `99`,
		},
		{
			name: "empty path returns entire document",
			data: []byte(`{"x": 1}`),
			path: nil,
			want: `{"x": 1}`,
		},
		{
			name: "null data returns null",
			data: nil,
			path: []string{"key"},
			want: `null`,
		},
		{
			name: "empty data returns null",
			data: []byte{},
			path: []string{"key"},
			want: `null`,
		},
		{
			name: "JSON null literal returns null",
			data: []byte(`null`),
			path: []string{"key"},
			want: `null`,
		},
		{
			name:    "missing field",
			data:    []byte(`{"name": "Alice"}`),
			path:    []string{"missing"},
			wantErr: true,
		},
		{
			name:    "nested missing field",
			data:    []byte(`{"user": {"name": "Alice"}}`),
			path:    []string{"user", "missing"},
			wantErr: true,
		},
		{
			name:    "non-object at path",
			data:    []byte(`{"name": "Alice"}`),
			path:    []string{"name", "sub"},
			wantErr: true,
		},
		{
			name: "object value",
			data: []byte(`{"user": {"id": 1, "name": "Alice"}}`),
			path: []string{"user"},
			want: `{"id": 1, "name": "Alice"}`,
		},
		{
			name: "array value",
			data: []byte(`{"items": [1, 2, 3]}`),
			path: []string{"items"},
			want: `[1, 2, 3]`,
		},
		{
			name: "skips earlier fields",
			data: []byte(`{"a": 1, "b": 2, "c": 3}`),
			path: []string{"c"},
			want: `3`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractPath(tt.data, tt.path...)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", string(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("ExtractPath = %q, want %q", string(got), tt.want)
			}
		})
	}
}

func TestSumFloat64(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []float64{5}, 5},
		{"multiple", []float64{1, 2, 3}, 6},
		{"negatives", []float64{-1, 2, -3}, -2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SumFloat64(tt.values); got != tt.want {
				t.Errorf("SumFloat64 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCountFloat64(t *testing.T) {
	if got := CountFloat64(nil); got != 0 {
		t.Errorf("CountFloat64(nil) = %v, want 0", got)
	}
	if got := CountFloat64([]float64{1, 2, 3}); got != 3 {
		t.Errorf("CountFloat64([1,2,3]) = %v, want 3", got)
	}
}

func TestMinFloat64(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"min at start", []float64{1, 5, 3}, 1},
		{"min at end", []float64{5, 3, 1}, 1},
		{"negatives", []float64{-1, -5, 0}, -5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MinFloat64(tt.values); got != tt.want {
				t.Errorf("MinFloat64 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaxFloat64(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"single", []float64{7}, 7},
		{"max at end", []float64{1, 5, 9}, 9},
		{"max at start", []float64{9, 3, 1}, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaxFloat64(tt.values); got != tt.want {
				t.Errorf("MaxFloat64 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAverageFloat64(t *testing.T) {
	if got := AverageFloat64(nil); got != 0 {
		t.Errorf("AverageFloat64(nil) = %v, want 0", got)
	}
	if got := AverageFloat64([]float64{2, 4, 6}); got != 4 {
		t.Errorf("AverageFloat64([2,4,6]) = %v, want 4", got)
	}
}

func TestProviderDepKey(t *testing.T) {
	dep := ProviderDep{Provider: "user_svc", Endpoint: "profile"}
	if got := dep.Key(); got != "user_svc.profile" {
		t.Errorf("Key() = %q, want %q", got, "user_svc.profile")
	}
}
