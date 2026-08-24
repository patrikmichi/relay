package agentport

import "testing"

func TestValidateName(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"git-helper", false},
		{"a", false},
		{"a1-b2-c3", false},
		{"", true},
		{"Git-Helper", true},             // uppercase
		{"git_helper", true},             // underscore
		{"-git-helper", true},            // leading hyphen
		{"git-helper-", true},            // trailing hyphen
		{"git--helper", true},            // doubled hyphen
		{"has space", true},              // space
		{string(make([]byte, 65)), true}, // too long (all NUL, also invalid chars, but length check first)
	}

	for _, c := range cases {
		err := ValidateName(c.name)
		if c.wantErr && err == nil {
			t.Errorf("ValidateName(%q): expected error, got nil", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("ValidateName(%q): unexpected error: %v", c.name, err)
		}
	}
}

func TestValidateNameMaxLength(t *testing.T) {
	// Exactly 64 lowercase letters — valid.
	name := ""
	for i := 0; i < 64; i++ {
		name += "a"
	}
	if err := ValidateName(name); err != nil {
		t.Errorf("64-char name should be valid: %v", err)
	}

	// 65 chars — invalid.
	name += "a"
	if err := ValidateName(name); err == nil {
		t.Errorf("65-char name should be invalid")
	}
}
