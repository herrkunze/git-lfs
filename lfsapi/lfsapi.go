package lfsapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/ThomsonReutersEikon/go-ntlm/ntlm"
	"github.com/git-lfs/git-lfs/errors"
)

var (
	lfsMediaTypeRE  = regexp.MustCompile(`\Aapplication/vnd\.git\-lfs\+json(;|\z)`)
	jsonMediaTypeRE = regexp.MustCompile(`\Aapplication/json(;|\z)`)
)

type Client struct {
	Endpoints   EndpointFinder
	Credentials CredentialHelper
	Netrc       NetrcFinder

	DialTimeout         int
	KeepaliveTimeout    int
	TLSTimeout          int
	ConcurrentTransfers int
	HTTPSProxy          string
	HTTPProxy           string
	NoProxy             string
	SkipSSLVerify       bool

	Verbose          bool
	DebuggingVerbose bool
	LoggingStats     bool
	VerboseOut       io.Writer

	hostClients map[string]*http.Client
	clientMu    sync.Mutex

	ntlmSessions map[string]ntlm.ClientSession
	ntlmMu       sync.Mutex

	transferBuckets  map[string][]*http.Response
	transferBucketMu sync.Mutex
	transfers        map[*http.Response]*httpTransfer
	transferMu       sync.Mutex

	// only used for per-host ssl certs
	gitEnv Env
	osEnv  Env
}

func NewClient(osEnv Env, gitEnv Env) (*Client, error) {
	if osEnv == nil {
		osEnv = make(TestEnv)
	}

	if gitEnv == nil {
		gitEnv = make(TestEnv)
	}

	netrc, err := ParseNetrc(osEnv)
	if err != nil {
		return nil, err
	}

	httpsProxy, httpProxy, noProxy := getProxyServers(osEnv, gitEnv)

	c := &Client{
		Endpoints: NewEndpointFinder(gitEnv),
		Credentials: &commandCredentialHelper{
			SkipPrompt: !osEnv.Bool("GIT_TERMINAL_PROMPT", true),
		},
		Netrc:               netrc,
		DialTimeout:         gitEnv.Int("lfs.dialtimeout", 0),
		KeepaliveTimeout:    gitEnv.Int("lfs.keepalive", 0),
		TLSTimeout:          gitEnv.Int("lfs.tlstimeout", 0),
		ConcurrentTransfers: gitEnv.Int("lfs.concurrenttransfers", 0),
		SkipSSLVerify:       !gitEnv.Bool("http.sslverify", true) || osEnv.Bool("GIT_SSL_NO_VERIFY", false),
		Verbose:             osEnv.Bool("GIT_CURL_VERBOSE", false),
		DebuggingVerbose:    osEnv.Bool("LFS_DEBUG_HTTP", false),
		LoggingStats:        osEnv.Bool("GIT_LOG_STATS", false),
		HTTPSProxy:          httpsProxy,
		HTTPProxy:           httpProxy,
		NoProxy:             noProxy,
		gitEnv:              gitEnv,
		osEnv:               osEnv,
	}

	return c, nil
}

func (c *Client) GitEnv() Env {
	return c.gitEnv
}

func (c *Client) OSEnv() Env {
	return c.osEnv
}

func IsDecodeTypeError(err error) bool {
	_, ok := err.(*decodeTypeError)
	return ok
}

type decodeTypeError struct {
	Type string
}

func (e *decodeTypeError) TypeError() {}

func (e *decodeTypeError) Error() string {
	return fmt.Sprintf("Expected json type, got: %q", e.Type)
}

func DecodeJSON(res *http.Response, obj interface{}) error {
	ctype := res.Header.Get("Content-Type")
	if !(lfsMediaTypeRE.MatchString(ctype) || jsonMediaTypeRE.MatchString(ctype)) {
		return &decodeTypeError{Type: ctype}
	}

	err := json.NewDecoder(res.Body).Decode(obj)
	res.Body.Close()

	if err != nil {
		return errors.Wrapf(err, "Unable to parse HTTP response for %s %s", res.Request.Method, res.Request.URL)
	}

	return nil
}

func sanitizedURL(u *url.URL) string {
	u2 := *u
	u2.RawQuery = ""
	if u.User != nil {
		u2.User = url.User(u.User.Username())
	}

	return (&u2).String()
}

// Env is an interface for the config.Environment methods that this package
// relies on.
type Env interface {
	Get(string) (string, bool)
	Int(string, int) int
	Bool(string, bool) bool
	All() map[string]string
}

// TestEnv is a basic config.Environment implementation. Only used in tests, or
// as a zero value to NewClient().
type TestEnv map[string]string

func (e TestEnv) Get(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

func (e TestEnv) Int(key string, def int) (val int) {
	s, _ := e.Get(key)
	if len(s) == 0 {
		return def
	}

	i, err := strconv.Atoi(s)
	if err != nil {
		return def
	}

	return i
}

func (e TestEnv) Bool(key string, def bool) (val bool) {
	s, _ := e.Get(key)
	if len(s) == 0 {
		return def
	}

	switch strings.ToLower(s) {
	case "true", "1", "on", "yes", "t":
		return true
	case "false", "0", "off", "no", "f":
		return false
	default:
		return false
	}
}

func (e TestEnv) All() map[string]string {
	return e
}