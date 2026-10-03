package main

import (
	"os"
	"reflect"
	"testing"
)

func TestParseTargets(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"http://orders:8080", []string{"http://orders:8080"}},
		{" http://a:8080/ , ,https://b.example ", []string{"http://a:8080", "https://b.example"}},
	}
	for _, tt := range tests {
		if got := parseTargets(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseTargets(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseNamespaceList(t *testing.T) {
	body, err := os.ReadFile("testdata/namespacelist.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseNamespaceList(body, "lab.patoarchitekci.io")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://team-a.lab.patoarchitekci.io", "https://team-b.lab.patoarchitekci.io"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	empty, err := parseNamespaceList([]byte(`{"kind":"NamespaceList","apiVersion":"v1","metadata":{},"items":[]}`), "x")
	if err != nil || len(empty) != 0 {
		t.Errorf("empty list = %v, %v", empty, err)
	}
	// A Status (e.g. 403 body) is not a namespace list.
	if _, err := parseNamespaceList([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":403}`), "x"); err == nil {
		t.Error("Status body: want error")
	}
}

func TestParseOrderResponse(t *testing.T) {
	v, e := parseOrderResponse([]byte(`{"id":"ab12","versions":{"orders":"1.0.7","inventory":"1.0.6","payments":"1.0.7"}}`))
	want := map[string]string{"orders": "1.0.7", "inventory": "1.0.6", "payments": "1.0.7"}
	if !reflect.DeepEqual(v, want) || e != "" {
		t.Errorf("201 body = %v, %q", v, e)
	}
	v, e = parseOrderResponse([]byte(`{"error":"payments: connection refused","upstream":"payments"}`))
	if v != nil || e != "payments: connection refused" {
		t.Errorf("502 body = %v, %q", v, e)
	}
	if _, e = parseOrderResponse([]byte("<html>502 Bad Gateway</html>")); e == "" {
		t.Error("HTML body: want error")
	}
}
