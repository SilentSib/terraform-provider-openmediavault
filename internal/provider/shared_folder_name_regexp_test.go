package provider

import "testing"

// TestShareNameRegexp pins the corrected "sharename" validator (see the
// doc comment on shareNameRegexp) against the MS-FSCC spec datamodel/
// schema.inc actually implements. An earlier version of this regex
// excluded the space character from the allowed set entirely, which
// incorrectly rejected valid share names containing internal spaces (the
// source's own comment explicitly calls out that spaces WITHIN a name are
// legal -- only leading/trailing space is forbidden).
func TestShareNameRegexp(t *testing.T) {
	valid := []string{
		"media",
		"a",
		"My Shared Folder",
		"a b c",
		"a  b", // multiple consecutive internal spaces
		"backups-2026",
	}
	invalid := []string{
		"",
		" ",
		" leading",
		"trailing ",
		"has/slash",
		`has"quote`,
		"has:colon",
		"has|pipe",
		"has<less",
		"has>greater",
		"has+plus",
		"has=equals",
		"has;semicolon",
		"has,comma",
		"has*star",
		"has?question",
		"has[bracket",
		"has]bracket",
		"has\\backslash",
		"has\x01control",
	}

	for _, name := range valid {
		if !shareNameRegexp.MatchString(name) {
			t.Errorf("expected %q to be a valid share name, but the regex rejected it", name)
		}
	}
	for _, name := range invalid {
		if shareNameRegexp.MatchString(name) {
			t.Errorf("expected %q to be an INVALID share name, but the regex accepted it", name)
		}
	}
}
