package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAlertsLoadAndValidate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	src := `history_max = 500

[[alerts]]
url_secret = "slack-ci"
on = "failure"
repos = ["baney75/site"]

[[alerts]]
url = "https://ntfy.sh/tentmaker-ci"
on = "slow"
min_duration = "20m"
`
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p, 16<<30, 8)
	if err != nil {
		t.Fatal(err)
	}
	if c.HistoryMax != 500 || len(c.Alerts) != 2 || c.Alerts[0].URLSecret != "slack-ci" ||
		c.Alerts[1].MinDuration != Duration(20*time.Minute) || c.Alerts[0].Repos[0] != "baney75/site" {
		t.Fatalf("loaded %+v", c)
	}
	if p := c.Validate(); len(p) != 0 {
		t.Fatalf("problems: %v", p)
	}
}

func TestAlertProblems(t *testing.T) {
	secretURL := "https://hooks.slack.com/services/T000/B000/sekrit"
	cases := []struct {
		a    Alert
		want string
	}{
		{Alert{On: "failure"}, "exactly one"},
		{Alert{URL: secretURL, URLSecret: "x", On: "failure"}, "exactly one"},
		{Alert{URLSecret: "Bad Name", On: "failure"}, "secret name"},
		{Alert{URL: "ftp://example.com/x?k=sekrit", On: "failure"}, "http or https"},
		{Alert{URL: secretURL, On: "sometimes"}, "must be"},
		{Alert{URL: secretURL, On: "slow"}, "min_duration"},
	}
	for _, tc := range cases {
		c := Default(16<<30, 8)
		c.Alerts = []Alert{tc.a}
		p := strings.Join(c.Validate(), "\n")
		if !strings.Contains(p, tc.want) {
			t.Errorf("%+v: problems %q miss %q", tc.a, p, tc.want)
		}
		if strings.Contains(p, "sekrit") {
			t.Errorf("a problem quotes the URL: %q", p)
		}
	}
	c := Default(16<<30, 8)
	c.HistoryMax = -1
	if p := strings.Join(c.Validate(), "\n"); !strings.Contains(p, "history_max") {
		t.Fatalf("problems %q", p)
	}
}
