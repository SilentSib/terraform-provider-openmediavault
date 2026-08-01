package omvclient

import "testing"

func TestParseMajorVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "8.0.14-1", want: 8},
		{in: "8.0.14", want: 8},
		{in: "9.1", want: 9},
		{in: "7.6.0-3", want: 7},
		{in: "10.0", want: 10},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	}

	for _, tc := range cases {
		got, err := parseMajorVersion(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMajorVersion(%q) = %d, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMajorVersion(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseMajorVersion(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestNewValidatesRequiredFields(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when Host is empty")
	}
	if _, err := New(Config{Host: "nas.local"}); err == nil {
		t.Fatal("expected error when Username is empty")
	}
	if _, err := New(Config{Host: "nas.local", Username: "admin", Scheme: "ftp"}); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
	c, err := New(Config{Host: "nas.local", Username: "admin"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.baseURL != "https://nas.local:443/rpc.php" {
		t.Errorf("unexpected default baseURL: %s", c.baseURL)
	}
}
