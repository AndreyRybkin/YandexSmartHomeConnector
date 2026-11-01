package main

import "testing"

func TestParseLine(t *testing.T) {
	cases := []struct {
		in       string
		action   string
		resource string
		version  int
		data     string
	}{
		{"GET /ce/hubInformation", "GET", "/ce/hubInformation", 1, ""},
		{"get /ci/services", "GET", "/ci/services", 1, ""},
		{"POST /ce/hubInformation 2 {\"hub\":\"CN-PRODUCTION\"}", "POST", "/ce/hubInformation", 2, `{"hub":"CN-PRODUCTION"}`},
		{"POST /ro/values {\"uid\": 1, \"value\": 2}", "POST", "/ro/values", 1, `{"uid": 1, "value": 2}`},
		{"NOTIFY /x 5", "NOTIFY", "/x", 5, ""},
	}
	for _, c := range cases {
		action, resource, version, data, err := parseLine(c.in, 1)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if action != c.action || resource != c.resource || version != c.version || string(data) != c.data {
			t.Errorf("%q -> %s %s v%d %q, ждали %s %s v%d %q", c.in,
				action, resource, version, string(data), c.action, c.resource, c.version, c.data)
		}
	}
}

func TestParseLineErrors(t *testing.T) {
	for _, in := range []string{"GET", "", "POST /x {не json}"} {
		if _, _, _, _, err := parseLine(in, 1); err == nil {
			t.Errorf("%q: ждали ошибку", in)
		}
	}
}
