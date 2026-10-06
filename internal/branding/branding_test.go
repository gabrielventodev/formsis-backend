package branding

import "testing"

func TestCheckColor(t *testing.T) {
	for _, ok := range []string{"#18181B", "#1d4ed8", "#000000", "#b91c1c"} {
		if _, err := CheckColor(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"#fff", "red", "#ffff00", "#60a5fa", "#ffffff", "#12345g"} {
		if _, err := CheckColor(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if got := ContrastWithWhite("#000000"); got < 20.9 || got > 21.1 {
		t.Errorf("black contrast %v", got)
	}
}
