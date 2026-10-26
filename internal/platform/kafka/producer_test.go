package kafka

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewProducerValidatesArguments(t *testing.T) {
	_, err := NewProducer(nil, "expense")
	require.Error(t, err)

	_, err = NewProducer([]string{"127.0.0.1:9092"}, "")
	require.Error(t, err)
}
