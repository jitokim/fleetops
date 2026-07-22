package domain

import "testing"

// TestAccount_IsDefault pins IsDefault as a pure derivation of ConfigDir, not
// an independently-settable flag that could drift from it.
func TestAccount_IsDefault(t *testing.T) {
	cases := []struct {
		name string
		acc  Account
		want bool
	}{
		{"empty config dir is default", Account{}, true},
		{"non-empty config dir is not default", Account{ConfigDir: "/home/user/.claude-work"}, false},
		{"alias/email set but config dir empty is STILL default", Account{ConfigDir: "", Alias: "company", Email: "x@y.com"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.acc.IsDefault(); got != c.want {
				t.Errorf("IsDefault() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAccount_Label_Precedence pins the display precedence spelled out in
// Label's doc: alias > email > short config-dir form > "" for default.
func TestAccount_Label_Precedence(t *testing.T) {
	cases := []struct {
		name string
		acc  Account
		want string
	}{
		{
			name: "default account (zero value) shows nothing — the zero-noise guarantee",
			acc:  Account{},
			want: "",
		},
		{
			name: "alias wins over email and config dir",
			acc:  Account{ConfigDir: "/home/user/.claude-work", Alias: "company", Email: "jito@company.com"},
			want: "company",
		},
		{
			name: "email wins when no alias is registered",
			acc:  Account{ConfigDir: "/home/user/.claude-work", Email: "jito@company.com"},
			want: "jito@company.com",
		},
		{
			name: "short config-dir form when neither alias nor email is known",
			acc:  Account{ConfigDir: "/home/user/.claude-work"},
			want: ".claude-work",
		},
		{
			name: "short config-dir form strips a trailing slash",
			acc:  Account{ConfigDir: "/home/user/.claude-work/"},
			want: ".claude-work",
		},
		{
			name: "default account NEVER shows alias/email, defensively — even if a future caller populated them for ConfigDir==\"\"",
			acc:  Account{ConfigDir: "", Alias: "should-not-show", Email: "should-not-show@x.com"},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.acc.Label(); got != c.want {
				t.Errorf("Label() = %q, want %q", got, c.want)
			}
		})
	}
}
