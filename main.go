// PicoFlare - Cloudflare Code Mode MCP + R2 + Vectorize.
// See https://github.com/cloudflare/mcp
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/joho/godotenv"

	"github.com/bigneek/picoflare/pkg/bot"
	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/memory"
	"github.com/bigneek/picoflare/pkg/storage"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found: %v", err)
	}

	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	r2AccessKey := os.Getenv("R2_ACCESS_KEY_ID")
	r2SecretKey := os.Getenv("R2_SECRET_ACCESS_KEY")
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")

	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "bot":
		runBot(bot.Config{
			TelegramToken:  telegramToken,
			AccountID:      accountID,
			APIToken:       apiToken,
			R2AccessKey:    r2AccessKey,
			R2SecretKey:    r2SecretKey,
			R2Bucket:       "pico-flare",
			VectorizeIndex: "picoflare-memory",
		})
		return
	case "mcp-test":
		runMCPTest(accountID, apiToken)
		return
	}

	// Default: quick connectivity test
	mcp := mcpclient.NewClient("https://mcp.cloudflare.com/mcp", apiToken, accountID)
	resp, err := mcp.SendLLMRequest(context.Background(), "Hello, PicoFlare")
	if err != nil {
		log.Fatalf("MCP request failed: %v", err)
	}
	fmt.Println("MCP:", resp)

	// R2 storage (if credentials set)
	if accountID != "" && r2AccessKey != "" && r2SecretKey != "" {
		r2, err := storage.NewR2Client(accountID, r2AccessKey, r2SecretKey)
		if err != nil {
			log.Printf("R2 client init failed: %v", err)
		} else {
			ctx := context.Background()
			bucket := "pico-flare"
			key := "test/hello.txt"
			data := []byte("Hello from PicoFlare")
			if err := r2.UploadObject(ctx, bucket, key, data); err != nil {
				log.Printf("R2 upload failed (bucket may not exist): %v", err)
			} else {
				down, err := r2.DownloadObject(ctx, bucket, key)
				if err != nil {
					log.Printf("R2 download failed: %v", err)
				} else {
					fmt.Printf("R2: uploaded and downloaded %q\n", string(down))
				}
			}
		}
	}

	// Vectorize memory (if credentials set)
	if accountID != "" && apiToken != "" {
		mem := memory.NewClient(accountID, apiToken)
		ctx := context.Background()
		indexName := "picoflare-memory"
		testVector := []float64{0.1, 0.2, 0.3, 0.4, 0.5}
		if err := mem.InsertVector(ctx, indexName, "test-1", testVector, map[string]string{"source": "main"}); err != nil {
			log.Printf("Vectorize insert failed (index may not exist): %v", err)
		} else {
			matches, err := mem.QueryVector(ctx, indexName, testVector, 3)
			if err != nil {
				log.Printf("Vectorize query failed: %v", err)
			} else {
				fmt.Printf("Vectorize: %d matches\n", len(matches))
				for _, m := range matches {
					fmt.Printf("  - id=%s score=%.4f\n", m.ID, m.Score)
				}
			}
		}
	}

	fmt.Println("PicoFlare initialized.")
}

func runMCPTest(accountID, apiToken string) {
	if accountID == "" || apiToken == "" {
		log.Fatalf("CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN required for mcp-test (accountID=%q, tokenLen=%d)", accountID, len(apiToken))
	}

	ctx := context.Background()
	mcp := mcpclient.NewClient("https://mcp.cloudflare.com/mcp", apiToken, accountID)

	// 0. Initialize
	fmt.Println("--- Initialize ---")
	if err := mcp.Initialize(ctx); err != nil {
		log.Fatalf("Initialize failed: %v", err)
	}
	fmt.Println("Initialized OK")

	// 1. Search for R2 bucket creation endpoint
	fmt.Println("--- Search: R2 buckets (create/list) ---")
	searchCode := `async () => {
		const results = [];
		for (const [path, methods] of Object.entries(spec.paths)) {
			for (const [method, op] of Object.entries(methods)) {
				if (typeof op !== 'object' || !op) continue;
				const pathLower = path.toLowerCase();
				if (pathLower.includes('/r2/') && pathLower.includes('bucket')) {
					results.push({ method: method.toUpperCase(), path, summary: op.summary });
				}
			}
		}
		return results.slice(0, 20);
	}`
	out, err := mcp.Search(ctx, searchCode)
	if err != nil {
		log.Fatalf("MCP search failed: %v", err)
	}
	fmt.Println(out)

	// 2. Create R2 bucket via MCP execute
	// Note: API token needs "Account - R2 - Edit" permission
	fmt.Println("\n--- Execute: Create R2 bucket pico-flare ---")
	createBucketCode := `async () => {
		const response = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/r2/buckets",
			body: { name: "pico-flare" }
		});
		return response;
	}`
	out, err = mcp.Execute(ctx, createBucketCode, accountID)
	if err != nil {
		log.Printf("Create bucket failed (ensure token has R2 Edit permission): %v", err)
	} else {
		fmt.Println(out)
	}

	// 3. Create Vectorize index via MCP execute
	fmt.Println("\n--- Execute: Create Vectorize index picoflare-memory ---")
	createIndexCode := `async () => {
		const response = await cloudflare.request({
			method: "POST",
			path: "/accounts/" + accountId + "/vectorize/v2/indexes",
			body: {
				name: "picoflare-memory",
				description: "PicoFlare RAG memory",
				config: { dimensions: 768, metric: "cosine" }
			}
		});
		return response;
	}`
	out, err = mcp.Execute(ctx, createIndexCode, accountID)
	if err != nil {
		log.Printf("Create Vectorize index failed (may already exist): %v", err)
	} else {
		fmt.Println(out)
	}

	fmt.Println("\n--- mcp-test done ---")
}

func runBot(cfg bot.Config) {
	if cfg.TelegramToken == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is required for bot mode")
	}
	b, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("Bot init failed: %v", err)
	}
	if err := b.Run(); err != nil {
		log.Fatalf("Bot error: %v", err)
	}
}
