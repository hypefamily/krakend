package krakend

import (
	"encoding/json"
	v1 "github.com/nais/krakend/api/v1"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/yaml"
	"os"
	"testing"
)

// TODO: add testcases
func TestParseKrakendEndpointsSpec(t *testing.T) {
	endpoints := &v1.ApiEndpoints{}
	err := parseYaml("testdata/apiendpoints.yaml", endpoints)
	assert.NoError(t, err)

	k := &v1.Krakend{}
	err = parseYaml("testdata/krakend.yaml", k)
	assert.NoError(t, err)

	partials, err := parseKrakendEndpointsSpec(k, endpoints.Spec)
	assert.NoError(t, err)

	_, err = json.Marshal(partials)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(partials))
	p := partials[0]
	assert.Equal(t, "/echo", p.Endpoint)
	assert.Equal(t, "GET", p.Method)
	assert.Equal(t, "2s", p.Timeout)
	assert.Equal(t, "/", p.Backend[0].UrlPattern)
	assert.Equal(t, "GET", p.Backend[0].Method)
	assert.Equal(t, "http://echo:1027", p.Backend[0].Host[0])
	assert.Equal(t, "org1:team1:krakend.app", p.ExtraConfig.AuthValidator.Scope[0])
	assert.Equal(t, "scope", p.ExtraConfig.AuthValidator.ScopesKey)
	assert.Equal(t, "https://test.maskinporten.no/jwk", p.ExtraConfig.AuthValidator.JwkUrl)
	assert.Equal(t, "https://test.maskinporten.no/", p.ExtraConfig.AuthValidator.Issuer)
	assert.Equal(t, "RS256", p.ExtraConfig.AuthValidator.Alg)
	assert.Equal(t, 10, p.ExtraConfig.QosRatelimitRouter.MaxRate)
	assert.Equal(t, 0, p.ExtraConfig.QosRatelimitRouter.ClientMaxRate)
	assert.Equal(t, "ip", p.ExtraConfig.QosRatelimitRouter.Strategy)
	assert.Equal(t, 0, p.ExtraConfig.QosRatelimitRouter.Capacity)
	assert.Equal(t, 0, p.ExtraConfig.QosRatelimitRouter.ClientCapacity)
	assert.Equal(t, "foo", p.InputQueryStrings[0])
	assert.Equal(t, "bar", p.InputQueryStrings[1])

	p2 := partials[1]
	assert.Equal(t, "/doc", p2.Endpoint)
	assert.Equal(t, "GET", p2.Method)
	assert.Equal(t, "/doc", p2.Backend[0].UrlPattern)
	assert.Equal(t, "GET", p2.Backend[0].Method)
	assert.Equal(t, "http://echo:1027", p2.Backend[0].Host[0])
	assert.Empty(t, p2.ExtraConfig.AuthValidator)
}

func parseYaml(file string, v any) error {
	reader, err := os.Open(file)
	if err != nil {
		return err
	}
	decoder := yaml.NewYAMLOrJSONDecoder(reader, 4096)
	err = decoder.Decode(v)
	if err != nil {
		return err
	}
	return nil
}

func TestParsePartials(t *testing.T) {
	content, err := os.ReadFile("testdata/config.json")
	assert.NoError(t, err)

	partials, err := ParsePartials(content)
	assert.NoError(t, err)

	assert.Equal(t, 2, len(partials.Endpoints))
}

func TestParseKrakendEndpointsSpec_RateLimits(t *testing.T) {
	spec := v1.ApiEndpointsSpec{
		AppName: "test-krakend-whois",
		Krakend: "api-gw",
		Auth: v1.Auth{
			Name:     "some-jwt-auth-provider",
			Cache:    true,
			Debug:    true,
			Audience: []string{"audience1"},
			Scope:    []string{"scope1"},
		},
		RateLimit: &v1.RateLimit{
			MaxRate:        1111,
			ClientMaxRate:  0,
			Strategy:       "ip",
			Capacity:       0,
			ClientCapacity: 0,
		},
		OpenEndpoints: []v1.Endpoint{
			{
				Path:        "/",
				Method:      "GET",
				BackendHost: "http://test-krakend-whoami",
				BackendPath: "/",
				RateLimit: &v1.RateLimit{
					MaxRate:        9999,
					ClientMaxRate:  0,
					Strategy:       "ip",
					Capacity:       0,
					ClientCapacity: 0,
				},
			},
			{
				Path:        "/test-no-rate-limits",
				Method:      "GET",
				BackendHost: "http://test-krakend-whoami",
				BackendPath: "/",
			},
		},
	}

	k := &v1.Krakend{
		Spec: v1.KrakendSpec{
			AuthProviders: []v1.AuthProvider{
				{
					Name:   "some-jwt-auth-provider",
					Alg:    "RS256",
					JwkUrl: "https://mock-jwk-url",
					Issuer: "https://mock-issuer",
				},
			},
		},
	}

	endpoints, err := parseKrakendEndpointsSpec(k, spec)
	assert.NoError(t, err)
	assert.Len(t, endpoints, 2)

	assert.Equal(t, "/", endpoints[0].Endpoint)
	assert.NotNil(t, endpoints[0].ExtraConfig)
	assert.NotNil(t, endpoints[0].ExtraConfig.QosRatelimitRouter)
	assert.Equal(t, 9999, endpoints[0].ExtraConfig.QosRatelimitRouter.MaxRate)
	assert.Equal(t, "ip", endpoints[0].ExtraConfig.QosRatelimitRouter.Strategy)

	assert.Equal(t, "/test-no-rate-limits", endpoints[1].Endpoint)
	assert.NotNil(t, endpoints[1].ExtraConfig)
	assert.NotNil(t, endpoints[1].ExtraConfig.QosRatelimitRouter)
	assert.Equal(t, 1111, endpoints[1].ExtraConfig.QosRatelimitRouter.MaxRate)
	assert.Equal(t, "ip", endpoints[1].ExtraConfig.QosRatelimitRouter.Strategy)
}
