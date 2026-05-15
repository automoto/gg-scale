package dashboard

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateUserDisableTarget_rejects_self(t *testing.T) {
	assert.ErrorIs(t, validateUserDisableTarget(7, 7), errCannotDisableSelf)
}

func TestValidateUserDisableTarget_accepts_other(t *testing.T) {
	assert.NoError(t, validateUserDisableTarget(7, 9))
}
