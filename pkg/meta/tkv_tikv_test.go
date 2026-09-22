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
