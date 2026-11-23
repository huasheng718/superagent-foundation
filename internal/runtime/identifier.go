package runtime

import "agentflow/internal/domain"

func safeID(v string) string {
	return domain.RuntimeResourceName(v)
}
