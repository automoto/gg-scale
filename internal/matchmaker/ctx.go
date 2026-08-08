package matchmaker

import (
	"context"

	"github.com/automoto/gg-scale/internal/db"
)

func tenantFromCtx(ctx context.Context) (int64, error) {
	return db.TenantFromContext(ctx)
}
