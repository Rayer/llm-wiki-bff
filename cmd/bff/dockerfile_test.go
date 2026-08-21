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
	if strings.Contains(dockerfile, "COPY --chmod") {
		t.Fatal("Dockerfile must not use BuildKit-only COPY --chmod")
	}
	for _, want := range []string{
		"cp -R configs/query/. /runtime-configs/query/",
		"find /runtime-configs/query -type d -exec chmod 0555 {} +",
		"find /runtime-configs/query -type f -exec chmod 0444 {} +",
		"COPY --from=build /runtime-configs/query /app/configs/query",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile is missing query config staging contract %q", want)
		}
	}
	if strings.Contains(dockerfile, "ENV QUERY_STAGE_CONFIG_PATH") || strings.Contains(dockerfile, "DEEPSEEK_API_KEY") || strings.Contains(dockerfile, "JWT_SECRET") {
		t.Fatal("Dockerfile bakes runtime config path or credentials")
	}
}

func TestDockerfileDisablesCGOForGenerationAndBuild(t *testing.T) {
	data, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	if !strings.Contains(dockerfile, "RUN CGO_ENABLED=0 go generate ./... && CGO_ENABLED=0 go build") {
		t.Fatal("Dockerfile must disable CGO for both generation and build")
	}
}
