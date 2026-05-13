package matchmaker

import (
	"context"

	"github.com/ggscale/ggscale/internal/db"
)

func tenantFromCtx(ctx context.Context) (int64, error) {
	return db.TenantFromContext(ctx)
}
