// Package branding holds an organization's look (name, color, logo) used by
// the applicant portal and the emails, stored in organizations.branding.
package branding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultColor is used when the organization has not picked one.
const DefaultColor = "#18181b"

// Settings is the JSON stored in organizations.branding.
type Settings struct {
	PrimaryColor string `json:"primary_color,omitempty"`
	SupportEmail string `json:"support_email,omitempty"`
	LogoKey      string `json:"logo_key,omitempty"`
	LogoMime     string `json:"logo_mime,omitempty"`
	LogoVersion  int    `json:"logo_version,omitempty"`
}

// Brand is an organization's resolved branding.
type Brand struct {
	Name string
	Settings
}

// Color returns the primary color, or the default.
func (b Brand) Color() string {
	if b.PrimaryColor == "" {
		return DefaultColor
	}
	return b.PrimaryColor
}

// LogoPath is the public path of the logo (cache-busted), or "" without one.
func (b Brand) LogoPath() string {
	if b.LogoKey == "" {
		return ""
	}
	return "/api/v1/branding/logo?v=" + strconv.Itoa(b.LogoVersion)
}

type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Load reads an organization's name and branding.
func Load(ctx context.Context, q Queryer, orgID string) (Brand, error) {
	var b Brand
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT name, branding FROM organizations WHERE id::text = $1`, orgID).Scan(&b.Name, &raw); err != nil {
		return b, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &b.Settings); err != nil {
			return b, err
		}
	}
	return b, nil
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// ErrLowContrast means white text would be hard to read on the color.
var ErrLowContrast = errors.New("low contrast")

// CheckColor validates a #RRGGBB color that buttons use behind white text:
// it must reach the WCAG AA contrast ratio of 4.5:1 against white.
func CheckColor(c string) (string, error) {
	if !hexColor.MatchString(c) {
		return "", fmt.Errorf("color inválido: usa el formato #RRGGBB")
	}
	c = strings.ToLower(c)
	if ContrastWithWhite(c) < 4.5 {
		return "", ErrLowContrast
	}
	return c, nil
}

// ContrastWithWhite returns the WCAG contrast ratio of white over the color.
func ContrastWithWhite(c string) float64 {
	lin := func(h string) float64 {
		v, _ := strconv.ParseUint(h, 16, 8)
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	l := 0.2126*lin(c[1:3]) + 0.7152*lin(c[3:5]) + 0.0722*lin(c[5:7])
	return 1.05 / (l + 0.05)
}
