//go:build goexperiment.jsonv2

package runtime

import (
	"reflect"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"
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

func TestLookupPath(t *testing.T) {
	tests := []struct {
		name string
		data any
		path []string
		want any
	}{
		{
			name: "empty path returns original value",
			data: map[string]any{"auth": "Bearer token"},
			path: nil,
			want: map[string]any{"auth": "Bearer token"},
		},
		{
			name: "top-level value",
			data: map[string]any{"auth": "Bearer token"},
			path: []string{"auth"},
			want: "Bearer token",
		},
		{
			name: "nested value",
			data: map[string]any{
				"user": map[string]any{
					"id":   "user-123",
					"auth": "Bearer token",
				},
			},
			path: []string{"user", "id"},
			want: "user-123",
		},
		{
			name: "missing path returns nil",
			data: map[string]any{"auth": "Bearer token"},
			path: []string{"missing"},
			want: nil,
		},
		{
			name: "scalar with extra path returns nil",
			data: map[string]any{"id": "user-123"},
			path: []string{"id", "nested"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LookupPath(tt.data, tt.path...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LookupPath(%v, %v) = %#v, want %#v", tt.data, tt.path, got, tt.want)
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

func TestProviderDepCacheKey(t *testing.T) {
	tests := []struct {
		name string
		dep  ProviderDep
		want string
	}{
		{
			name: "minimal GET",
			dep:  ProviderDep{Provider: "svc", Endpoint: "ep"},
			want: "svc.ep:GET::0",
		},
		{
			name: "explicit GET",
			dep:  ProviderDep{Provider: "svc", Endpoint: "ep", Method: "GET"},
			want: "svc.ep:GET::0",
		},
		{
			name: "POST with empty body",
			dep:  ProviderDep{Provider: "svc", Endpoint: "ep", Method: "POST"},
			want: "svc.ep:POST::0",
		},
		{
			name: "with headers",
			dep: ProviderDep{
				Provider: "svc", Endpoint: "ep",
				Headers: map[string]string{"A": "1", "B": "2"},
			},
			want: "svc.ep:GET:A:1|B:2:0",
		},
		{
			name: "with body",
			dep:  ProviderDep{Provider: "svc", Endpoint: "ep", Body: []byte(`{"x":1}`)},
			want: "svc.ep:GET::", // hash suffix varies; we only check prefix and that it's non-zero
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.dep.CacheKey()
			if tt.name == "with body" {
				if !hasPrefix(got, "svc.ep:GET::") || got == "svc.ep:GET::0" {
					t.Errorf("CacheKey() with body = %q, want prefix svc.ep:GET:: and non-zero body hash", got)
				}
				return
			}
			if got != tt.want {
				t.Errorf("CacheKey() = %q, want %q", got, tt.want)
			}
		})
	}
	// Header order independence: same headers in different order yield same key
	a := ProviderDep{Provider: "p", Endpoint: "e", Headers: map[string]string{"X": "1", "Y": "2"}}
	b := ProviderDep{Provider: "p", Endpoint: "e", Headers: map[string]string{"Y": "2", "X": "1"}}
	if a.CacheKey() != b.CacheKey() {
		t.Errorf("CacheKey() should be order-independent: %q vs %q", a.CacheKey(), b.CacheKey())
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestLookupJSON(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		path []string
		want any
	}{
		{
			name: "valid path",
			data: []byte(`{"user": {"name": "Alice", "age": 30}}`),
			path: []string{"user", "name"},
			want: "Alice",
		},
		{
			name: "missing path returns nil",
			data: []byte(`{"a": 1}`),
			path: []string{"missing"},
			want: nil,
		},
		{
			name: "invalid JSON returns nil",
			data: []byte(`{invalid`),
			path: []string{"key"},
			want: nil,
		},
		{
			name: "empty path returns root",
			data: []byte(`{"x": 42}`),
			path: nil,
			want: map[string]any{"x": float64(42)},
		},
		{
			name: "null value",
			data: []byte(`{"n": null}`),
			path: []string{"n"},
			want: nil,
		},
		{
			name: "number",
			data: []byte(`{"count": 100}`),
			path: []string{"count"},
			want: float64(100),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LookupJSON(tt.data, tt.path...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LookupJSON(...) = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNewHTTPErrorAndToResponse(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		message    string
		code       []string
		wantCode   string
	}{
		{
			name: "inferred 500",
			statusCode: 500,
			message:    "server error",
			code:       nil,
			wantCode:   ErrorCodeInternalError,
		},
		{
			name: "inferred 400",
			statusCode: 400,
			message:    "bad request",
			code:       nil,
			wantCode:   ErrorCodeInvalidRequest,
		},
		{
			name: "502 falls under >=500",
			statusCode: 502,
			message:    "bad gateway",
			code:       nil,
			wantCode:   ErrorCodeInternalError,
		},
		{
			name: "504 falls under >=500",
			statusCode: 504,
			message:    "gateway timeout",
			code:       nil,
			wantCode:   ErrorCodeInternalError,
		},
		{
			name: "explicit code",
			statusCode: 418,
			message:    "teapot",
			code:       []string{"TEAPOT"},
			wantCode:   "TEAPOT",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var he *HTTPError
			if len(tt.code) > 0 {
				he = NewHTTPError(tt.statusCode, tt.message, tt.code[0])
			} else {
				he = NewHTTPError(tt.statusCode, tt.message)
			}
			if he.StatusCode != tt.statusCode || he.Message != tt.message {
				t.Errorf("NewHTTPError: StatusCode=%d Message=%q", he.StatusCode, he.Message)
			}
			if he.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", he.Code, tt.wantCode)
			}
			resp, err := he.ToResponse()
			if err != nil {
				t.Fatalf("ToResponse: %v", err)
			}
			if resp.StatusCode != tt.statusCode {
				t.Errorf("ToResponse StatusCode = %d, want %d", resp.StatusCode, tt.statusCode)
			}
			if resp.Headers["Content-Type"] != "application/json" {
				t.Errorf("Content-Type = %q", resp.Headers["Content-Type"])
			}
			if len(resp.Body) == 0 {
				t.Error("ToResponse body should not be empty")
			}
			// Body should be valid JSON with error, status, code
			var m map[string]any
			if err := jsonv2.Unmarshal(resp.Body, &m); err != nil {
				t.Fatalf("ToResponse body invalid JSON: %v", err)
			}
			if m["error"] != tt.message || m["code"] != tt.wantCode {
				t.Errorf("body = %v", m)
			}
			if status, ok := m["status"].(float64); !ok || int(status) != tt.statusCode {
				t.Errorf("body status = %v, want %d", m["status"], tt.statusCode)
			}
		})
	}
}

func TestPower(t *testing.T) {
	tests := []struct {
		name     string
		base     float64
		exponent float64
		want     float64
	}{
		{"2^3", 2, 3, 8},
		{"5^2", 5, 2, 25},
		{"10^0", 10, 0, 1},
		{"2^-1", 2, -1, 0.5},
		{"0^5", 0, 5, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Power(tt.base, tt.exponent); got != tt.want {
				t.Errorf("Power(%v, %v) = %v, want %v", tt.base, tt.exponent, got, tt.want)
			}
		})
	}
}

func TestSqrt(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		want float64
	}{
		{"sqrt(16)", 16, 4},
		{"sqrt(25)", 25, 5},
		{"sqrt(0)", 0, 0},
		{"sqrt(2)", 2, 1.4142135623730951},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sqrt(tt.v); got != tt.want {
				t.Errorf("Sqrt(%v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}

func TestRandom(t *testing.T) {
	// Test that Random() returns values in [0, 1) range
	for i := 0; i < 100; i++ {
		got := Random()
		if got < 0 || got >= 1 {
			t.Errorf("Random() = %v, want value in [0, 1)", got)
		}
	}
	// Test that we get different values (very unlikely to get 100 identical values)
	values := make(map[float64]bool)
	for i := 0; i < 100; i++ {
		values[Random()] = true
	}
	if len(values) < 10 {
		t.Errorf("Random() seems to be returning too few unique values, got %d unique values", len(values))
	}
}

func TestPad(t *testing.T) {
	tests := []struct {
		name    string
		str     any
		length  float64
		padChar []any
		want    string
	}{
		{"pad to 5 with space", "x", 5, nil, "x    "},
		{"pad to 5 with #", "x", 5, []any{"#"}, "x####"},
		{"already long enough", "hello", 3, nil, "hello"},
		{"exact length", "abc", 3, nil, "abc"},
		{"pad number", 42, 5, nil, "42   "},
		{"empty pad char uses space", "x", 3, []any{""}, "x  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if len(tt.padChar) > 0 {
				got = Pad(tt.str, tt.length, tt.padChar...)
			} else {
				got = Pad(tt.str, tt.length)
			}
			if got != tt.want {
				t.Errorf("Pad(%v, %v, %v) = %q, want %q", tt.str, tt.length, tt.padChar, got, tt.want)
			}
		})
	}
}

func TestSplitArray(t *testing.T) {
	tests := []struct {
		name      string
		str       any
		separator any
		want      []any
	}{
		{"split by comma", "a,b,c", ",", []any{"a", "b", "c"}},
		{"split by dash", "1-2-3", "-", []any{"1", "2", "3"}},
		{"empty separator", "abc", "", []any{"a", "b", "c"}},
		{"no separator found", "abc", "x", []any{"abc"}},
		{"split number", 12345, "", []any{"1", "2", "3", "4", "5"}},
		{"empty string", "", ",", []any{""}},
		{"separator at start", ",a,b", ",", []any{"", "a", "b"}},
		{"separator at end", "a,b,", ",", []any{"a", "b", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitArray(tt.str, tt.separator)
			gotArr, ok := got.([]any)
			if !ok {
				t.Fatalf("SplitArray returned %T, want []any", got)
			}
			if len(gotArr) != len(tt.want) {
				t.Errorf("SplitArray length = %d, want %d", len(gotArr), len(tt.want))
				return
			}
			for i, v := range gotArr {
				if v != tt.want[i] {
					t.Errorf("SplitArray[%d] = %v, want %v", i, v, tt.want[i])
				}
			}
		})
	}
}

func TestShuffleArray(t *testing.T) {
	// Test that shuffle returns same length
	arr := []any{1, 2, 3, 4, 5}
	shuffled := ShuffleArray(arr)
	shuffledArr, ok := shuffled.([]any)
	if !ok {
		t.Fatalf("ShuffleArray returned %T, want []any", shuffled)
	}
	if len(shuffledArr) != len(arr) {
		t.Errorf("ShuffleArray length = %d, want %d", len(shuffledArr), len(arr))
	}
	// Test that all elements are present (order may differ)
	originalMap := make(map[any]bool)
	for _, v := range arr {
		originalMap[v] = true
	}
	for _, v := range shuffledArr {
		if !originalMap[v] {
			t.Errorf("ShuffleArray contains unexpected element %v", v)
		}
	}
	// Test that non-array input is returned unchanged
	if got := ShuffleArray("not an array"); got != "not an array" {
		t.Errorf("ShuffleArray(non-array) = %v, want unchanged", got)
	}
	// Test empty array
	if got := ShuffleArray([]any{}); len(got.([]any)) != 0 {
		t.Errorf("ShuffleArray([]) length = %d, want 0", len(got.([]any)))
	}
}

func TestZipArray(t *testing.T) {
	tests := []struct {
		name   string
		arrays []any
		want   []any
	}{
		{
			"two arrays",
			[]any{[]any{1, 2}, []any{3, 4}},
			[]any{[]any{1, 3}, []any{2, 4}},
		},
		{
			"three arrays",
			[]any{[]any{1, 2}, []any{3, 4}, []any{5, 6}},
			[]any{[]any{1, 3, 5}, []any{2, 4, 6}},
		},
		{
			"different lengths (min wins)",
			[]any{[]any{1, 2, 3}, []any{4, 5}},
			[]any{[]any{1, 4}, []any{2, 5}},
		},
		{
			"empty arrays",
			[]any{[]any{}, []any{}},
			[]any{},
		},
		{
			"no arrays",
			[]any{},
			[]any{},
		},
		{
			"single array",
			[]any{[]any{1, 2, 3}},
			[]any{[]any{1}, []any{2}, []any{3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got any
			if len(tt.arrays) == 0 {
				got = ZipArray()
			} else {
				got = ZipArray(tt.arrays...)
			}
			gotArr, ok := got.([]any)
			if !ok {
				t.Fatalf("ZipArray returned %T, want []any", got)
			}
			if len(gotArr) != len(tt.want) {
				t.Errorf("ZipArray length = %d, want %d", len(gotArr), len(tt.want))
				return
			}
			for i, v := range gotArr {
				wantTuple := tt.want[i].([]any)
				gotTuple := v.([]any)
				if len(gotTuple) != len(wantTuple) {
					t.Errorf("ZipArray[%d] length = %d, want %d", i, len(gotTuple), len(wantTuple))
					continue
				}
				for j, tv := range gotTuple {
					if tv != wantTuple[j] {
						t.Errorf("ZipArray[%d][%d] = %v, want %v", i, j, tv, wantTuple[j])
					}
				}
			}
		})
	}
}

func TestValuesMap(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want []any
	}{
		{
			"simple object",
			map[string]any{"a": 1, "b": 2},
			[]any{1, 2}, // sorted by key
		},
		{
			"empty object",
			map[string]any{},
			[]any{},
		},
		{
			"mixed types",
			map[string]any{"x": "hello", "y": 42, "z": true},
			[]any{"hello", 42, true}, // sorted by key
		},
		{
			"not an object",
			"not an object",
			nil,
		},
		{
			"array",
			[]any{1, 2, 3},
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValuesMap(tt.v)
			if tt.want == nil {
				if got != nil {
					t.Errorf("ValuesMap(%v) = %v, want nil", tt.v, got)
				}
				return
			}
			gotArr, ok := got.([]any)
			if !ok {
				t.Fatalf("ValuesMap returned %T, want []any", got)
			}
			if len(gotArr) != len(tt.want) {
				t.Errorf("ValuesMap length = %d, want %d", len(gotArr), len(tt.want))
				return
			}
			// Values are returned in sorted key order
			for i, v := range gotArr {
				if v != tt.want[i] {
					t.Errorf("ValuesMap[%d] = %v, want %v", i, v, tt.want[i])
				}
			}
		})
	}
}

func TestSpreadMap(t *testing.T) {
	// SpreadMap returns the object itself
	obj := map[string]any{"a": 1, "b": 2}
	got := SpreadMap(obj)
	if !reflect.DeepEqual(got, obj) {
		t.Errorf("SpreadMap should return the same object, got %v, want %v", got, obj)
	}
	// Non-object input is returned unchanged
	if got := SpreadMap("not an object"); got != "not an object" {
		t.Errorf("SpreadMap(non-object) = %v, want unchanged", got)
	}
	if got := SpreadMap([]any{1, 2}); got.([]any)[0] != 1 {
		t.Errorf("SpreadMap(array) = %v, want unchanged", got)
	}
}

func TestNow(t *testing.T) {
	got := Now()
	// Should be a valid ISO 8601 timestamp
	_, err := time.Parse(time.RFC3339Nano, got)
	if err != nil {
		t.Errorf("Now() returned invalid ISO 8601 format: %q, error: %v", got, err)
	}
	// Should end with Z (UTC)
	if len(got) == 0 || got[len(got)-1] != 'Z' {
		t.Errorf("Now() should return UTC timestamp ending with Z, got %q", got)
	}
}

func TestMillis(t *testing.T) {
	got := Millis()
	// Should be a reasonable timestamp (after 2020-01-01)
	minMillis := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if got < float64(minMillis) {
		t.Errorf("Millis() = %v, want >= %v", got, minMillis)
	}
	// Should be close to current time (within 1 second)
	now := time.Now().UnixMilli()
	if got < float64(now-1000) || got > float64(now+1000) {
		t.Errorf("Millis() = %v, want close to %v", got, now)
	}
}

func TestFromMillis(t *testing.T) {
	tests := []struct {
		name     string
		ms       float64
		picture  []string
		want     string // We'll check it's valid ISO 8601
		wantTime time.Time
	}{
		{
			"basic conversion",
			1705315800123,
			nil,
			"",
			time.UnixMilli(1705315800123).UTC(),
		},
		{
			"with picture (ignored)",
			1705315800123,
			[]string{"YYYY-MM-DD"},
			"",
			time.UnixMilli(1705315800123).UTC(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if len(tt.picture) > 0 {
				got = FromMillis(tt.ms, tt.picture...)
			} else {
				got = FromMillis(tt.ms)
			}
			// Parse and verify it matches expected time
			parsed, err := time.Parse(time.RFC3339Nano, got)
			if err != nil {
				t.Errorf("FromMillis returned invalid ISO 8601: %q, error: %v", got, err)
				return
			}
			if !parsed.Equal(tt.wantTime) {
				t.Errorf("FromMillis time = %v, want %v", parsed, tt.wantTime)
			}
		})
	}
}

func TestToMillis(t *testing.T) {
	tests := []struct {
		name     string
		timestamp any
		picture  []string
		want     float64
	}{
		{
			"RFC3339Nano format",
			"2024-01-15T10:30:00.123456789Z",
			nil,
			1705314600123,
		},
		{
			"RFC3339 format",
			"2024-01-15T10:30:00Z",
			nil,
			1705314600000,
		},
		{
			"with picture (ignored)",
			"2024-01-15T10:30:00Z",
			[]string{"YYYY-MM-DD"},
			1705314600000,
		},
		{
			"invalid format returns 0",
			"invalid",
			nil,
			0,
		},
		{
			"number coerced to string",
			1705315800123,
			nil,
			0, // Will fail to parse as ISO 8601
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got float64
			if len(tt.picture) > 0 {
				got = ToMillis(tt.timestamp, tt.picture...)
			} else {
				got = ToMillis(tt.timestamp)
			}
			if got != tt.want {
				t.Errorf("ToMillis(%v, %v) = %v, want %v", tt.timestamp, tt.picture, got, tt.want)
			}
		})
	}
}
