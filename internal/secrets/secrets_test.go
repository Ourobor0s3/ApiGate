package secrets

import "testing"

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"NEWS_API_KEY":     true,
		"WEATHER_LOCATION": true,
		"MAIN_CURRENCY":    true,
		"a-b_c":            true,
		"x1":               true,
		"":                 false,
		"has space":        false,
		"a/b":              false,
		"ключ":             false,
	}
	for name, want := range cases {
		if got := validName.MatchString(name); got != want {
			t.Errorf("validName(%q) = %v, want %v", name, got, want)
		}
	}
}
