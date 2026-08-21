package main

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfileCopiesReviewedQueryConfigWithoutBakingEnv(t *testing.T) {
	data, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	if !strings.Contains(dockerfile, "COPY --chmod=0444 --from=build /src/configs/query /app/configs/query") {
		t.Fatal("Dockerfile does not copy configs/query into the runtime image with mode 0444")
	}
	if strings.Contains(dockerfile, "ENV QUERY_STAGE_CONFIG_PATH") || strings.Contains(dockerfile, "DEEPSEEK_API_KEY") {
		t.Fatal("Dockerfile bakes runtime config path or credentials")
	}
}
