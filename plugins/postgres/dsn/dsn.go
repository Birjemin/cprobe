package dsn

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// DSN represents a parsed datasource. It contains fields for the individual connection components.
type DSN struct {
	scheme   string
	username string
	password string
	host     string
	path     string
	query    url.Values
}

func (d *DSN) SetUserAndOptions(user, pass string, options map[string]string) {
	d.username = user
	d.password = pass
	for k, v := range options {
		d.query.Set(k, v)
	}
}

// String makes a dsn safe to print by excluding any passwords. This allows dsn to be used in
// strings and log messages without needing to call a redaction function first.
func (d DSN) String() string {
	if d.password != "" {
		return fmt.Sprintf("%s://%s:******@%s%s?%s", d.scheme, d.username, d.host, d.path, d.query.Encode())
	}

	if d.username != "" {
		return fmt.Sprintf("%s://%s@%s%s?%s", d.scheme, d.username, d.host, d.path, d.query.Encode())
	}

	return fmt.Sprintf("%s://%s%s?%s", d.scheme, d.host, d.path, d.query.Encode())
}

// GetConnectionString returns the URL to pass to the driver for database connections. This value should not be logged.
func (d DSN) GetConnectionString() string {
	u := url.URL{
		Scheme:   d.scheme,
		Host:     d.host,
		Path:     d.path,
		RawQuery: d.query.Encode(),
	}

	// Username and Password
	if d.username != "" {
		u.User = url.UserPassword(d.username, d.password)
	}

	return u.String()
}

// dsnFromString parses a connection string into a dsn. It will attempt to parse the string as
// a URL and as a set of key=value pairs. If both attempts fail, dsnFromString will return an error.
func DsnFromString(in string) (DSN, error) {
	if strings.HasPrefix(in, "postgresql://") || strings.HasPrefix(in, "postgres://") {
		return dsnFromURL(in)
	}

	// Try to parse as key=value pairs
	d, err := dsnFromKeyValue(in)
	if err == nil {
		return d, nil
	}

	// Parse the string as a URL, with the scheme prefixed
	d, err = dsnFromURL(fmt.Sprintf("postgresql://%s", in))
	if err == nil {
		return d, nil
	}

	return DSN{}, fmt.Errorf("could not understand DSN")
}

// dsnFromURL parses the input as a URL and returns the dsn representation.
func dsnFromURL(in string) (DSN, error) {
	u, err := url.Parse(in)
	if err != nil {
		return DSN{}, err
	}
	pass, _ := u.User.Password()
	user := u.User.Username()

	query := u.Query()

	if queryPass := query.Get("password"); queryPass != "" {
		if pass == "" {
			pass = queryPass
		}
	}
	query.Del("password")

	if queryUser := query.Get("user"); queryUser != "" {
		if user == "" {
			user = queryUser
		}
	}
	query.Del("user")

	d := DSN{
		scheme:   u.Scheme,
		username: user,
		password: pass,
		host:     u.Host,
		path:     u.Path,
		query:    query,
	}

	return d, nil
}

// dsnFromKeyValue parses the input as a set of key=value pairs and returns the dsn representation.
func dsnFromKeyValue(in string) (DSN, error) {
	// Attempt to confirm at least one key=value pair before starting the rune parser
	connstringRe := regexp.MustCompile(`^ *[a-zA-Z0-9]+ *= *[^= ]+`)
	if !connstringRe.MatchString(in) {
		return DSN{}, fmt.Errorf("input is not a key-value DSN")
	}

	// Anything other than known fields should be part of the querystring
	query := url.Values{}

	pairs, err := parseKeyValue(in)
	if err != nil {
		return DSN{}, fmt.Errorf("failed to parse key-value DSN: %v", err)
	}

	// Build the dsn from the key=value pairs
	d := DSN{
		scheme: "postgresql",
	}

	hostname := ""
	port := ""

	for k, v := range pairs {
		switch k {
		case "host":
			hostname = v
		case "port":
			port = v
		case "user":
			d.username = v
		case "password":
			d.password = v
		default:
			query.Set(k, v)
		}
	}

	if hostname == "" {
		hostname = "localhost"
	}

	if port == "" {
		d.host = hostname
	} else {
		d.host = fmt.Sprintf("%s:%s", hostname, port)
	}

	d.query = query

	return d, nil
}

// parseKeyValue is a key=value parser. It loops over each rune to split out keys and values
// and attempting to honor quoted values. parseKeyValue will return an error if it is unable
// to properly parse the input.
func parseKeyValue(in string) (map[string]string, error) {
	out := map[string]string{}

	inPart := false
	inQuote := false
	part := []rune{}
	key := ""
	for _, c := range in {
		switch {
		case unicode.In(c, unicode.Quotation_Mark):
			if inQuote {
				inQuote = false
			} else {
				inQuote = true
			}
		case unicode.In(c, unicode.White_Space):
			if inPart {
				if inQuote {
					part = append(part, c)
				} else {
					// Are we finishing a key=value?
					if key == "" {
						return out, fmt.Errorf("invalid input")
					}
					out[key] = string(part)
					inPart = false
					part = []rune{}
				}
			} else {
				// Are we finishing a key=value?
				if key == "" {
					return out, fmt.Errorf("invalid input")
				}
				out[key] = string(part)
				inPart = false
				part = []rune{}
				// Do something with the value
			}
		case c == '=':
			if inPart {
				inPart = false
				key = string(part)
				part = []rune{}
			} else {
				return out, fmt.Errorf("invalid input")
			}
		default:
			inPart = true
			part = append(part, c)
		}
	}

	if key != "" && len(part) > 0 {
		out[key] = string(part)
	}

	return out, nil
}
