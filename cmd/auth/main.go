package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/rayer/llm-wiki-bff/internal/auth"
	"github.com/rayer/llm-wiki-bff/internal/buildinfo"
	"github.com/rayer/llm-wiki-bff/internal/config"
	firestoreclient "github.com/rayer/llm-wiki-bff/internal/firestore"
	"github.com/rayer/llm-wiki-bff/internal/middleware"
	"github.com/rayer/llm-wiki-bff/internal/observability"
	"github.com/rayer/llm-wiki-bff/internal/syssettings"
)

func main() {
	localFlag := flag.String("local", "", "local data directory")
	flag.Parse()

	cfg, err := config.Load(".")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	localDataDir := strings.TrimSpace(*localFlag)
	if localDataDir == "" {
		localDataDir = strings.TrimSpace(cfg.LocalDataDir)
	}
	if localDataDir == "" {
		localDataDir = strings.TrimSpace(os.Getenv("LOCAL_DATA_DIR"))
	}
	localMode := localDataDir != ""

	var fsClient *firestoreclient.Client
	if localMode {
		log.Printf("Local mode: Firestore client disabled")
	} else {
		fsClient, err = firestoreclient.NewClientWithDatabase(cfg.GCPProject, cfg.FirestoreDatabaseID, "", "")
		if err != nil {
			log.Printf("WARNING: Firestore client not available: %v", err)
		}
	}

	var settingsStore *syssettings.Store
	if !localMode && fsClient != nil && fsClient.Raw() != nil {
		settingsStore = syssettings.NewStore(fsClient.Raw(), cfg.RegistrationEnabled)
	} else {
		settingsStore = syssettings.NewStore(nil, cfg.RegistrationEnabled)
	}

	provider, err := observability.InitMetrics(context.Background(), observabilityServiceName(os.Getenv("K_SERVICE")), observability.GetProjectID())
	if err != nil {
		log.Printf("[observability] WARNING: metrics init failed (continuing): %v", err)
	} else {
		defer func() {
			if err := provider.Shutdown(context.Background()); err != nil {
				log.Printf("[observability] metrics shutdown error: %v", err)
			}
		}()
	}

	r := newProductionRouter(cfg, localMode, fsClient, settingsStore)

	log.Printf("Auth service listening on :%s", cfg.Port)
	log.Fatal(r.Run(":" + cfg.Port))
}

func newProductionRouter(cfg config.Config, localMode bool, fsClient *firestoreclient.Client, settingsStore syssettings.RegistrationGate) *gin.Engine {
	r := gin.Default()
	r.Use(authHostAllowlist(cfg, localMode))
	r.Use(middleware.SecurityHeaders(!cfg.DevJWT))
	r.Use(middleware.LatencyMiddleware())

	r.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.AllowedOriginsFor(localMode),
		AllowMethods:     []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowHeaders:     []string{"Content-Type"},
		AllowCredentials: true,
	}))

	authRoutes := r.Group("/api/v1/auth")
	authRoutes.Use(authRequestBodyLimit())
	if localMode {
		authRoutes.POST("/login", middleware.NewRateLimiter(10, time.Minute), auth.LocalDevLoginHandler(cfg.JWTSecret))
		authRoutes.POST("/register", func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "registration is disabled in local mode; use demo@llm-wiki.dev / demo123456"})
		})
		authRoutes.POST("/refresh", auth.LocalDevRefreshHandler(cfg.JWTSecret))
		authRoutes.POST("/logout", auth.LogoutHandlerWithCookiePolicy(auth.LocalRefreshCookiePolicy()))
	} else if fsClient == nil || fsClient.Raw() == nil {
		unavailable := func(c *gin.Context) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth routes require Firestore"})
		}
		authRoutes.POST("/login", unavailable)
		authRoutes.POST("/register", unavailable)
		authRoutes.POST("/refresh", unavailable)
		authRoutes.POST("/logout", auth.LogoutHandlerWithCookiePolicy(auth.HostRefreshCookiePolicy()))
	} else {
		authRoutes.POST("/login", middleware.NewRateLimiter(10, time.Minute), auth.LoginHandlerWithCookiePolicy(fsClient.Raw(), cfg.JWTSecret, auth.HostRefreshCookiePolicy()))
		authRoutes.POST("/register", middleware.NewRateLimiter(5, time.Minute), auth.RegisterHandler(fsClient.Raw(), cfg.JWTSecret, settingsStore))
		authRoutes.POST("/refresh", auth.RefreshHandlerWithCookiePolicy(fsClient.Raw(), cfg.JWTSecret, auth.HostRefreshCookiePolicy()))
		authRoutes.POST("/logout", auth.LogoutHandlerWithCookiePolicy(auth.HostRefreshCookiePolicy()))
	}

	r.GET("/healthz", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	r.GET("/api/v1/public/version", buildinfo.Handler())

	return r
}

const maxAuthRequestBodyBytes = 64 << 10

func authRequestBodyLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthRequestBodyBytes)
		}
		c.Next()
	}
}

func authHostAllowlist(cfg config.Config, localMode bool) gin.HandlerFunc {
	allowed := make(map[string]struct{})
	for _, host := range cfg.AllowedHostsFor(localMode) {
		allowed[strings.ToLower(host)] = struct{}{}
	}
	return func(c *gin.Context) {
		host := requestHost(c.Request.Host)
		if (!localMode && isLocalHost(host)) || host == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid Host header"})
			return
		}
		if _, ok := allowed[host]; !ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid Host header"})
			return
		}
		c.Next()
	}
}

func requestHost(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if host, _, err := net.SplitHostPort(raw); err == nil {
		return host
	}
	return raw
}

func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1"
}

const legacyAuthObservabilityServiceName = "llm-wiki-auth-dev"

func observabilityServiceName(kService string) string {
	if serviceName := strings.TrimSpace(kService); serviceName != "" {
		return serviceName
	}
	return legacyAuthObservabilityServiceName
}
