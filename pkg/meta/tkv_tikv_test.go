//go:build !notikv
// +build !notikv

package meta

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRequiredUint32(t *testing.T) {
	re := require.New(t)
	query := url.Values{"namespace-id": []string{"123"}}

	value, err := parseRequiredUint32(query, "namespace-id")
	re.NoError(err)
	re.Equal(uint32(123), value)

	_, err = parseRequiredUint32(url.Values{}, "namespace-id")
	re.Error(err)

	_, err = parseRequiredUint32(url.Values{"namespace-id": []string{"0"}}, "namespace-id")
	re.Error(err)

	_, err = parseRequiredUint32(url.Values{"namespace-id": []string{"4294967296"}}, "namespace-id")
	re.Error(err)
}

func TestTransientUnformattedRefreshOnlyForAPIV3(t *testing.T) {
	v3 := withPrefix(&tikvClient{tolerateUnformattedRefresh: true}, []byte("p"))
	if !v3.(transientUnformattedRefresh).tolerateTransientUnformattedRefresh() {
		t.Fatal("API V3 client should tolerate a transient unformatted refresh")
	}
	defaultClient := withPrefix(&tikvClient{}, []byte("p"))
	if defaultClient.(transientUnformattedRefresh).tolerateTransientUnformattedRefresh() {
		t.Fatal("default client must not tolerate an unformatted refresh")
	}

	m := &kvMeta{baseMeta: &baseMeta{}, client: defaultClient}
	m.en = m
	if engineToleratesTransientUnformattedRefresh(m) {
		t.Fatal("default engine must exit on an unformatted refresh")
	}
	m.client = v3
	if !engineToleratesTransientUnformattedRefresh(m) {
		t.Fatal("API V3 engine should keep the existing format")
	}

	redisEngine := &redisMeta{baseMeta: &baseMeta{}}
	redisEngine.en = redisEngine
	if _, ok := any(redisEngine).(transientUnformattedRefresh); ok {
		t.Fatal("redisMeta must not opt in through baseMeta embedding")
	}
	if engineToleratesTransientUnformattedRefresh(redisEngine) {
		t.Fatal("redis refresh must not keep a missing format")
	}
	sqlEngine := &dbMeta{baseMeta: &baseMeta{}}
	sqlEngine.en = sqlEngine
	if _, ok := any(sqlEngine).(transientUnformattedRefresh); ok {
		t.Fatal("dbMeta must not opt in through baseMeta embedding")
	}
	if engineToleratesTransientUnformattedRefresh(sqlEngine) {
		t.Fatal("sql refresh must not keep a missing format")
	}
}
