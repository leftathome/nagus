package main

import (
	"testing"
	"time"

	"github.com/leftathome/nagus/internal/category"
)

// --- envOr ---

func TestEnvOrReturnsSetValue(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVOR", "explicit")
	if got := envOr("NAGUS_TEST_ENVOR", "default"); got != "explicit" {
		t.Errorf("envOr() = %q, want %q", got, "explicit")
	}
}

func TestEnvOrReturnsDefaultWhenUnset(t *testing.T) {
	key := "NAGUS_TEST_ENVOR_UNSET"
	if got := envOr(key, "default"); got != "default" {
		t.Errorf("envOr() = %q, want %q", got, "default")
	}
}

func TestEnvOrReturnsEmptyStringWhenExplicitlySetEmpty(t *testing.T) {
	// os.LookupEnv distinguishes "set but empty" from "unset"; envOr should
	// honor an explicitly empty value rather than falling back to def.
	t.Setenv("NAGUS_TEST_ENVOR_EMPTY", "")
	if got := envOr("NAGUS_TEST_ENVOR_EMPTY", "default"); got != "" {
		t.Errorf("envOr() = %q, want empty string", got)
	}
}

// --- envBool ---

func TestEnvBoolParsesValues(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want bool
	}{
		{"true lowercase", "true", true},
		{"1", "1", true},
		{"TRUE uppercase", "TRUE", true},
		{"t shorthand", "t", true},
		{"false lowercase", "false", false},
		{"0", "0", false},
		{"FALSE uppercase", "FALSE", false},
		{"f shorthand", "f", false},
		{"garbage defaults false", "not-a-bool", false},
		{"empty defaults false", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NAGUS_TEST_ENVBOOL", tc.val)
			if got := envBool("NAGUS_TEST_ENVBOOL"); got != tc.want {
				t.Errorf("envBool(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestEnvBoolUnsetIsFalse(t *testing.T) {
	if got := envBool("NAGUS_TEST_ENVBOOL_UNSET"); got != false {
		t.Errorf("envBool() unset = %v, want false", got)
	}
}

// --- envFloat ---

func TestEnvFloatParsesValidValue(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVFLOAT", "3.5")
	if got := envFloat("NAGUS_TEST_ENVFLOAT", 1.0); got != 3.5 {
		t.Errorf("envFloat() = %v, want 3.5", got)
	}
}

func TestEnvFloatUnsetReturnsDefault(t *testing.T) {
	if got := envFloat("NAGUS_TEST_ENVFLOAT_UNSET", 2.25); got != 2.25 {
		t.Errorf("envFloat() unset = %v, want 2.25", got)
	}
}

func TestEnvFloatMalformedReturnsDefault(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVFLOAT_BAD", "not-a-float")
	if got := envFloat("NAGUS_TEST_ENVFLOAT_BAD", 7.0); got != 7.0 {
		t.Errorf("envFloat() malformed = %v, want default 7.0", got)
	}
}

func TestEnvFloatNegativeAndZero(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVFLOAT_NEG", "-4.2")
	if got := envFloat("NAGUS_TEST_ENVFLOAT_NEG", 0); got != -4.2 {
		t.Errorf("envFloat() negative = %v, want -4.2", got)
	}
	t.Setenv("NAGUS_TEST_ENVFLOAT_ZERO", "0")
	if got := envFloat("NAGUS_TEST_ENVFLOAT_ZERO", 9.0); got != 0 {
		t.Errorf("envFloat() zero = %v, want 0", got)
	}
}

// --- envInt64 ---

func TestEnvInt64ParsesValidValue(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVINT64", "42")
	if got := envInt64("NAGUS_TEST_ENVINT64", 1); got != 42 {
		t.Errorf("envInt64() = %v, want 42", got)
	}
}

func TestEnvInt64UnsetReturnsDefault(t *testing.T) {
	if got := envInt64("NAGUS_TEST_ENVINT64_UNSET", 99); got != 99 {
		t.Errorf("envInt64() unset = %v, want 99", got)
	}
}

func TestEnvInt64MalformedReturnsDefault(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVINT64_BAD", "not-an-int")
	if got := envInt64("NAGUS_TEST_ENVINT64_BAD", 13); got != 13 {
		t.Errorf("envInt64() malformed = %v, want default 13", got)
	}
}

func TestEnvInt64RejectsFloatString(t *testing.T) {
	// ParseInt should fail on a float-looking string; default should surface.
	t.Setenv("NAGUS_TEST_ENVINT64_FLOAT", "3.5")
	if got := envInt64("NAGUS_TEST_ENVINT64_FLOAT", 5); got != 5 {
		t.Errorf("envInt64() float string = %v, want default 5", got)
	}
}

func TestEnvInt64NegativeValue(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVINT64_NEG", "-17")
	if got := envInt64("NAGUS_TEST_ENVINT64_NEG", 0); got != -17 {
		t.Errorf("envInt64() negative = %v, want -17", got)
	}
}

// --- envDuration ---

func TestEnvDurationParsesValidValue(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVDURATION", "5m")
	want := 5 * time.Minute
	if got := envDuration("NAGUS_TEST_ENVDURATION", time.Second); got != want {
		t.Errorf("envDuration() = %v, want %v", got, want)
	}
}

func TestEnvDurationUnsetReturnsDefault(t *testing.T) {
	want := 30 * time.Second
	if got := envDuration("NAGUS_TEST_ENVDURATION_UNSET", want); got != want {
		t.Errorf("envDuration() unset = %v, want %v", got, want)
	}
}

func TestEnvDurationMalformedReturnsDefault(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVDURATION_BAD", "not-a-duration")
	want := 10 * time.Minute
	if got := envDuration("NAGUS_TEST_ENVDURATION_BAD", want); got != want {
		t.Errorf("envDuration() malformed = %v, want default %v", got, want)
	}
}

func TestEnvDurationParsesComplexDuration(t *testing.T) {
	t.Setenv("NAGUS_TEST_ENVDURATION_COMPLEX", "1h30m")
	want := 90 * time.Minute
	if got := envDuration("NAGUS_TEST_ENVDURATION_COMPLEX", 0); got != want {
		t.Errorf("envDuration() = %v, want %v", got, want)
	}
}

// --- categoryOptsFromEnv ---

func pureClearCategoryEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"NAGUS_LAND_BUDGET_CENTS",
		"NAGUS_LAND_MIN_ACREAGE",
		"NAGUS_LAND_MAX_ACREAGE",
		"NAGUS_RENTCAST_KEY",
		"NAGUS_ZILLAPI_KEY",
		"NAGUS_EBAY_CLIENT_ID",
		"NAGUS_EBAY_CLIENT_SECRET",
	} {
		t.Setenv(k, "")
	}
}

func TestCategoryOptsFromEnvDefaults(t *testing.T) {
	pureClearCategoryEnv(t)
	got := categoryOptsFromEnv(true, nil, nil)
	if !got.hddOffline {
		t.Error("hddOffline: want true (passed through)")
	}
	if got.landBudgetCents != 0 {
		t.Errorf("landBudgetCents = %v, want 0", got.landBudgetCents)
	}
	if got.landMinAcreage != category.DefaultMinAcreageAcres {
		t.Errorf("landMinAcreage = %v, want %v", got.landMinAcreage, category.DefaultMinAcreageAcres)
	}
	if got.landMaxAcreage != 0 {
		t.Errorf("landMaxAcreage = %v, want 0", got.landMaxAcreage)
	}
	if got.rentcastKey != "" {
		t.Errorf("rentcastKey = %q, want empty", got.rentcastKey)
	}
	if got.zillapiKey != "" {
		t.Errorf("zillapiKey = %q, want empty", got.zillapiKey)
	}
	if got.ebayClientID != "" {
		t.Errorf("ebayClientID = %q, want empty", got.ebayClientID)
	}
	if got.ebaySecret != "" {
		t.Errorf("ebaySecret = %q, want empty", got.ebaySecret)
	}
}

func TestCategoryOptsFromEnvReadsAllFields(t *testing.T) {
	t.Setenv("NAGUS_LAND_BUDGET_CENTS", "500000")
	t.Setenv("NAGUS_LAND_MIN_ACREAGE", "1.5")
	t.Setenv("NAGUS_LAND_MAX_ACREAGE", "40")
	t.Setenv("NAGUS_RENTCAST_KEY", "rentcast-key")
	t.Setenv("NAGUS_ZILLAPI_KEY", "zillapi-key")
	t.Setenv("NAGUS_EBAY_CLIENT_ID", "client-id")
	t.Setenv("NAGUS_EBAY_CLIENT_SECRET", "client-secret")

	got := categoryOptsFromEnv(false, nil, nil)
	if got.hddOffline {
		t.Error("hddOffline: want false (passed through)")
	}
	if got.landBudgetCents != 500000 {
		t.Errorf("landBudgetCents = %v, want 500000", got.landBudgetCents)
	}
	if got.landMinAcreage != 1.5 {
		t.Errorf("landMinAcreage = %v, want 1.5", got.landMinAcreage)
	}
	if got.landMaxAcreage != 40 {
		t.Errorf("landMaxAcreage = %v, want 40", got.landMaxAcreage)
	}
	if got.rentcastKey != "rentcast-key" {
		t.Errorf("rentcastKey = %q, want rentcast-key", got.rentcastKey)
	}
	if got.zillapiKey != "zillapi-key" {
		t.Errorf("zillapiKey = %q, want zillapi-key", got.zillapiKey)
	}
	if got.ebayClientID != "client-id" {
		t.Errorf("ebayClientID = %q, want client-id", got.ebayClientID)
	}
	if got.ebaySecret != "client-secret" {
		t.Errorf("ebaySecret = %q, want client-secret", got.ebaySecret)
	}
}

// --- orDefault ---

func TestOrDefault(t *testing.T) {
	cases := []struct {
		name string
		s    string
		def  string
		want string
	}{
		{"empty returns default", "", "fallback", "fallback"},
		{"nonempty returns itself", "value", "fallback", "value"},
		{"both empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orDefault(tc.s, tc.def); got != tc.want {
				t.Errorf("orDefault(%q, %q) = %q, want %q", tc.s, tc.def, got, tc.want)
			}
		})
	}
}

// --- orInt ---

func TestOrInt(t *testing.T) {
	cases := []struct {
		name string
		n    int
		def  int
		want int
	}{
		{"zero returns default", 0, 20, 20},
		{"nonzero returns itself", 5, 20, 5},
		{"negative is not zero, returns itself", -3, 20, -3},
		{"both zero", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orInt(tc.n, tc.def); got != tc.want {
				t.Errorf("orInt(%v, %v) = %v, want %v", tc.n, tc.def, got, tc.want)
			}
		})
	}
}

// --- buildPostgresDSN ---

func TestBuildPostgresDSNAllFieldsSet(t *testing.T) {
	got := buildPostgresDSN("dbhost", "5432", "nagus", "nagususer", "s3cr3t", "require")
	want := "postgres://nagususer:s3cr3t@dbhost:5432/nagus?sslmode=require"
	if got != want {
		t.Errorf("buildPostgresDSN() = %q, want %q", got, want)
	}
}

func TestBuildPostgresDSNEmptyOptionalFields(t *testing.T) {
	// No user (so no credentials in the URL) and no sslmode (so no query string).
	got := buildPostgresDSN("dbhost", "5432", "nagus", "", "", "")
	want := "postgres://dbhost:5432/nagus"
	if got != want {
		t.Errorf("buildPostgresDSN() = %q, want %q", got, want)
	}
}

func TestBuildPostgresDSNEmptyPasswordWithUser(t *testing.T) {
	// A user with no password should still produce user info (empty password),
	// per url.UserPassword's semantics.
	got := buildPostgresDSN("dbhost", "5432", "nagus", "nagususer", "", "disable")
	want := "postgres://nagususer:@dbhost:5432/nagus?sslmode=disable"
	if got != want {
		t.Errorf("buildPostgresDSN() = %q, want %q", got, want)
	}
}

func TestBuildPostgresDSNEscapesSpecialCharacters(t *testing.T) {
	// Password containing characters that must be percent-encoded in a URL
	// userinfo component (@, :, /, spaces, #).
	got := buildPostgresDSN("dbhost", "5432", "nagus", "user name", "p@ss:w/ord #1", "require")
	want := "postgres://user%20name:p%40ss%3Aw%2Ford%20%231@dbhost:5432/nagus?sslmode=require"
	if got != want {
		t.Errorf("buildPostgresDSN() = %q, want %q", got, want)
	}
}

func TestBuildPostgresDSNEscapesDBNameWithSlash(t *testing.T) {
	got := buildPostgresDSN("dbhost", "5432", "my db", "u", "p", "")
	want := "postgres://u:p@dbhost:5432/my%20db"
	if got != want {
		t.Errorf("buildPostgresDSN() = %q, want %q", got, want)
	}
}

func TestBuildPostgresDSNIPv6Host(t *testing.T) {
	// net.JoinHostPort brackets IPv6 literals; confirm that survives into the DSN.
	got := buildPostgresDSN("::1", "5432", "nagus", "u", "p", "")
	want := "postgres://u:p@[::1]:5432/nagus"
	if got != want {
		t.Errorf("buildPostgresDSN() = %q, want %q", got, want)
	}
}
