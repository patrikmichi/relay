// Package redact removes secrets and personal data from a session on this
// machine, before anything is uploaded. The rules are the file
// redaction-rules.json; the gateway applies the same file again to what it
// receives.
package redact

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"sort"
	"strings"
)

//go:embed redaction-rules.json
var rulesJSON []byte

// RulesJSON is the rule file as embedded, for comparing with the gateway's copy.
func RulesJSON() []byte { return rulesJSON }

type rules struct {
	Format  string `json:"format"`
	Version string `json:"version"`
	Secrets []struct {
		Name    string `json:"name"`
		Pattern string `json:"pattern"`
	} `json:"secrets"`
	Assignment string `json:"assignment"`
	SecretKey  string `json:"secretKey"`
	Entropy    struct {
		MinLength int     `json:"minLength"`
		MinBits   float64 `json:"minBits"`
		Window    int     `json:"window"`
	} `json:"entropy"`
	URLCredential string   `json:"urlCredential"`
	URLParam      string   `json:"urlParam"`
	PathUser      []string `json:"pathUser"`
}

type compiled struct {
	version       string
	secrets       []*regexp.Regexp
	assignment    *regexp.Regexp
	secretKey     *regexp.Regexp
	entropy       *regexp.Regexp
	entropyMin    float64
	entropyWindow int
	urlCred       *regexp.Regexp
	urlParam      *regexp.Regexp
	paths         []*regexp.Regexp
}

// Classes of redaction, as counted in a report.
const (
	ClassSecret     = "secret"
	ClassEmail      = "email"
	ClassPhone      = "phone"
	ClassIBAN       = "iban"
	ClassCard       = "card"
	ClassNationalID = "national_id"
	ClassIP         = "ip"
	ClassURL        = "url_credential"
	ClassPath       = "path_user"
	ClassTerm       = "custom_term"
)

var placeholderName = map[string]string{
	ClassSecret: "SECRET", ClassEmail: "EMAIL", ClassPhone: "PHONE", ClassIBAN: "IBAN", ClassCard: "CARD",
	ClassNationalID: "ID", ClassIP: "IP", ClassURL: "URL", ClassTerm: "TERM",
}

var (
	rule = mustCompile(rulesJSON)

	emailRe  = regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)
	e164Re   = regexp.MustCompile(`\+\d{1,3}[\s.\-]?\(?\d{1,4}\)?[\s.\-]?\d{1,4}[\s.\-]?\d{1,9}`)
	nanpRe   = regexp.MustCompile(`\(\d{3}\)\s?\d{3}[\s.\-]\d{4}|\b\d{3}[\s.\-]\d{3}[\s.\-]\d{4}\b`)
	ssnRe    = regexp.MustCompile(`\b\d{3}[\s\-]\d{2}[\s\-]\d{4}\b`)
	cardRe   = regexp.MustCompile(`\b(?:\d[ \-]?){12,18}\d\b`)
	ibanRe   = regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{4,30}\b`)
	skRcRe   = regexp.MustCompile(`\b\d{6}/\d{3,4}\b`)
	ipv4Re   = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	ipv6Re   = regexp.MustCompile(`\b(?:[0-9A-Fa-f]{1,4}:){7}[0-9A-Fa-f]{1,4}\b`)
	numberRe = regexp.MustCompile(`\d`)
)

func mustCompile(raw []byte) *compiled {
	var r rules
	if err := json.Unmarshal(raw, &r); err != nil {
		panic(fmt.Sprintf("redaction rules: %v", err))
	}
	c := &compiled{version: r.Version, entropyMin: r.Entropy.MinBits, entropyWindow: r.Entropy.Window}
	for _, s := range r.Secrets {
		c.secrets = append(c.secrets, regexp.MustCompile(s.Pattern))
	}
	c.assignment = regexp.MustCompile(r.Assignment)
	c.secretKey = regexp.MustCompile(r.SecretKey)
	c.entropy = regexp.MustCompile(fmt.Sprintf(`[A-Za-z0-9_+/=\-]{%d,}`, r.Entropy.MinLength))
	c.urlCred = regexp.MustCompile(r.URLCredential)
	c.urlParam = regexp.MustCompile(r.URLParam)
	for _, p := range r.PathUser {
		c.paths = append(c.paths, regexp.MustCompile(p))
	}
	return c
}

// Redactor numbers placeholders consistently across everything it is given,
// so the same address is the same placeholder in every turn.
type Redactor struct {
	terms  []string
	nums   map[string]map[string]int
	counts map[string]int
}

// New returns a redactor that also removes the given terms, case-insensitively.
func New(terms []string) *Redactor {
	r := &Redactor{nums: map[string]map[string]int{}, counts: map[string]int{}}
	for _, t := range terms {
		if t = strings.TrimSpace(t); t != "" {
			r.terms = append(r.terms, t)
		}
	}
	sort.Slice(r.terms, func(i, j int) bool { return len(r.terms[i]) > len(r.terms[j]) })
	return r
}

// Counts is how many values of each class the redactor has replaced so far.
func (r *Redactor) Counts() map[string]int {
	out := make(map[string]int, len(r.counts))
	for k, v := range r.counts {
		out[k] = v
	}
	return out
}

// RulesVersion is the version of the embedded rule file.
func RulesVersion() string { return rule.version }

func (r *Redactor) placeholder(class, value string) string {
	m := r.nums[class]
	if m == nil {
		m = map[string]int{}
		r.nums[class] = m
	}
	n, ok := m[value]
	if !ok {
		n = len(m) + 1
		m[value] = n
	}
	r.counts[class]++
	return fmt.Sprintf("⟦%s_%d⟧", placeholderName[class], n)
}

// Text redacts one string.
func (r *Redactor) Text(s string) string {
	for _, t := range r.terms {
		s = replaceFold(s, t, func(m string) string { return r.placeholder(ClassTerm, strings.ToLower(m)) })
	}
	for _, re := range rule.secrets {
		s = re.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder(ClassSecret, m) })
	}
	s = rule.urlCred.ReplaceAllStringFunc(s, func(m string) string {
		sub := rule.urlCred.FindStringSubmatch(m)
		return sub[1] + r.placeholder(ClassURL, sub[2]+":"+sub[3]) + "@"
	})
	s = replaceGroup(s, rule.urlParam, func(v string) string { return r.placeholder(ClassURL, v) })
	s = replaceGroup(s, rule.assignment, func(v string) string {
		if strings.HasPrefix(v, "⟦") {
			return v
		}
		return r.placeholder(ClassSecret, v)
	})
	s = r.entropyTokens(s)
	s = r.pii(s)
	for _, re := range rule.paths {
		s = re.ReplaceAllStringFunc(s, func(string) string { r.counts[ClassPath]++; return "~" })
	}
	return s
}

// replaceGroup replaces group 2 of every match, keeping group 1.
func replaceGroup(s string, re *regexp.Regexp, fn func(string) string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		return sub[1] + fn(sub[2])
	})
}

func replaceFold(s, term string, fn func(string) string) string {
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(term))
	return re.ReplaceAllStringFunc(s, fn)
}

// entropyTokens redacts a long, random-looking token that follows a
// secret-like word closely, such as a key written on the line after its name.
func (r *Redactor) entropyTokens(s string) string {
	locs := rule.secretKey.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var out strings.Builder
	last := 0
	for _, loc := range locs {
		if loc[1] < last {
			continue
		}
		end := min(loc[1]+rule.entropyWindow, len(s))
		window := s[loc[1]:end]
		t := rule.entropy.FindStringIndex(window)
		if t == nil || strings.Contains(window[t[0]:t[1]], "⟦") {
			continue
		}
		tok := window[t[0]:t[1]]
		if shannon(tok) < rule.entropyMin {
			continue
		}
		start := loc[1] + t[0]
		out.WriteString(s[last:start])
		out.WriteString(r.placeholder(ClassSecret, tok))
		last = start + len(tok)
	}
	out.WriteString(s[last:])
	return out.String()
}

func shannon(s string) float64 {
	freq := map[rune]float64{}
	n := 0.0
	for _, c := range s {
		freq[c]++
		n++
	}
	h := 0.0
	for _, f := range freq {
		p := f / n
		h -= p * math.Log2(p)
	}
	return h
}

func (r *Redactor) pii(s string) string {
	s = emailRe.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder(ClassEmail, strings.ToLower(m)) })
	s = ssnRe.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder(ClassNationalID, m) })
	s = skRcRe.ReplaceAllStringFunc(s, func(m string) string {
		if validSkRc(m) {
			return r.placeholder(ClassNationalID, m)
		}
		return m
	})
	s = cardRe.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(m) {
			return r.placeholder(ClassCard, digits(m))
		}
		return m
	})
	s = ibanRe.ReplaceAllStringFunc(s, func(m string) string {
		if validIBAN(m) {
			return r.placeholder(ClassIBAN, m)
		}
		return m
	})
	s = e164Re.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder(ClassPhone, digits(m)) })
	s = nanpRe.ReplaceAllStringFunc(s, func(m string) string { return r.placeholder(ClassPhone, digits(m)) })
	s = ipv4Re.ReplaceAllStringFunc(s, func(m string) string { return r.ip(m) })
	return ipv6Re.ReplaceAllStringFunc(s, func(m string) string { return r.ip(m) })
}

// ip redacts an address outside the private, loopback and link-local ranges.
func (r *Redactor) ip(m string) string {
	a, err := netip.ParseAddr(m)
	if err != nil || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() {
		return m
	}
	return r.placeholder(ClassIP, m)
}

func digits(s string) string {
	return strings.Join(numberRe.FindAllString(s, -1), "")
}

func luhn(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if double {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

func validIBAN(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	re := s[4:] + s[:4]
	rem := 0
	for _, c := range re {
		var n int
		if c >= 'A' && c <= 'Z' {
			n = int(c-'A') + 10
			rem = (rem*100 + n) % 97
		} else {
			rem = (rem*10 + int(c-'0')) % 97
		}
	}
	return rem == 1
}

func validSkRc(s string) bool {
	c := strings.Replace(s, "/", "", 1)
	if len(c) < 9 || len(c) > 10 {
		return false
	}
	mm := int(c[2]-'0')*10 + int(c[3]-'0')
	return (mm >= 1 && mm <= 12) || (mm >= 51 && mm <= 62)
}
