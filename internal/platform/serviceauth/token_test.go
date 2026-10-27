package serviceauth

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestSignVerifyRoundTrip(t *testing.T) {
	tenantID := uuid.New()
	now := time.Now()
	token, err := Sign(testSecret, tenantID, time.Minute, now)
	require.NoError(t, err)

	got, err := Verify(testSecret, token, now.Add(30*time.Second))
	require.NoError(t, err)
	assert.Equal(t, tenantID, got)
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	token, err := Sign(testSecret, uuid.New(), time.Minute, time.Now())
	require.NoError(t, err)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 2)

	other, err := Sign(testSecret, uuid.New(), time.Minute, time.Now())
	require.NoError(t, err)
	otherParts := strings.Split(other, ".")
	tampered := otherParts[0] + "." + parts[1]

	_, err = Verify(testSecret, tampered, time.Now())
	assert.Error(t, err)
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	token, err := Sign(testSecret, uuid.New(), time.Minute, time.Now())
	require.NoError(t, err)
	_, err = Verify("ffffffffffffffffffffffffffffffff", token, time.Now())
	assert.Error(t, err)
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	now := time.Now()
	token, err := Sign(testSecret, uuid.New(), time.Minute, now)
	require.NoError(t, err)
	_, err = Verify(testSecret, token, now.Add(2*time.Minute))
	assert.Error(t, err)
}

func TestSignRejectsWeakSecret(t *testing.T) {
	_, err := Sign("short", uuid.New(), time.Minute, time.Now())
	assert.Error(t, err)
}

func TestVerifyRejectsMalformedToken(t *testing.T) {
	for _, token := range []string{"", "only-one-part", ".", "a.b.c", "!!.??"} {
		_, err := Verify(testSecret, token, time.Now())
		assert.Error(t, err, "token=%q", token)
	}
}
