package discovery

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCase1(t *testing.T) {
	url := "https://example.com/users/test-com/xxxx"
	re := regexp.MustCompile(`/users/([^/]+)/`)
	match := re.FindStringSubmatch(url)

	assert.True(t, len(match) > 1)
	assert.Equal(t, "test-com", match[1])
}

func TestCase2(t *testing.T) {
	url := "https://example.com/user/test-com/xxxx"
	re := regexp.MustCompile(`/users/([^/]+)/`)
	match := re.FindStringSubmatch(url)

	assert.True(t, len(match) == 0)
}

func TestCase3(t *testing.T) {
	url := "https://example.com/users/test-com"
	re := regexp.MustCompile(`/users/([^/]+)`)
	match := re.FindStringSubmatch(url)

	assert.True(t, len(match) > 1)
	assert.Equal(t, "test-com", match[1])
}

type MockDiscovery struct {
	appName string
}

func NewMockDiscovery(appName string) *MockDiscovery {
	return &MockDiscovery{appName: appName}
}

func (m MockDiscovery) PickService(ctx context.Context, serviceName string) (string, error) {
	if serviceName != m.appName {
		return "", fmt.Errorf("cannot found serviceName:%s", serviceName)
	}

	return "http://127.0.0.1:12007", nil
}

func TestHandler(t *testing.T) {
	result := "Hello, World!"
	appName := "test-mock.example"
	httpServer := &http.Server{
		Addr: "127.0.0.1:12007",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, result)
		}),
	}
	// start http server
	go func() {
		if err := httpServer.ListenAndServe(); err != nil {
			t.Fatal(err)
		}
	}()

	SetDefaultDiscovery(NewMockDiscovery(appName))

	testCases := []struct {
		desc   string
		rule   string
		target string
	}{
		{
			desc:   "test const rule",
			rule:   RuleConst + fmt.Sprintf("(`%s`)", appName),
			target: "http://example.com:9000/mock/test/xx?foo=ba",
		},
		{
			desc:   "test RulePathRegexp rule",
			rule:   RulePathRegexp + "(`/([^/]+)/`)",
			target: fmt.Sprintf("http://example.com:9000/%s/test/xx?foo=bar", appName),
		},
		{
			desc:   "test RulePathRegexp rule",
			rule:   RuleHeader + "(`test-header`)",
			target: "http://example.com:9000/mock/test/xx?foo=bar",
		},
	}
	for _, test := range testCases {
		t.Run(test.desc, func(t *testing.T) {
			server, err := New(nil, "test", test.rule, nil)
			assert.NoError(t, err)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, test.target, nil)
			req.Header.Set("test-header", appName)
			server.ServeHTTP(recorder, req)
			assert.True(t, recorder.Result() != nil)

			assert.Equal(t, http.StatusOK, recorder.Result().StatusCode)

			data, err := io.ReadAll(recorder.Result().Body)
			assert.NoError(t, err)
			assert.Equal(t, result, string(data))
		})
	}
}
