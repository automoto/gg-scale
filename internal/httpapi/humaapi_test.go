package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestServerError_status(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nul_byte_is_client_input", fmt.Errorf("wrap: %w", &pgconn.PgError{Code: "22021"}), http.StatusBadRequest},
		{"other_pg_error", &pgconn.PgError{Code: "40001"}, http.StatusInternalServerError},
		{"plain_error", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var se huma.StatusError
			errors.As(serverError(context.Background(), "test", tt.err), &se)
			assert.Equal(t, tt.want, se.GetStatus())
		})
	}
}
