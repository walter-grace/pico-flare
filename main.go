// PicoFlare - Cloudflare Code Mode MCP + R2 + Vectorize.
// See https://github.com/cloudflare/mcp
package main

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/bigneek/picoflare/pkg/agent"
	"github.com/bigneek/picoflare/pkg/bot"
	cf "github.com/bigneek/picoflare/pkg/cloudflare"
	"github.com/bigneek/picoflare/pkg/llm"
	"github.com/bigneek/picoflare/pkg/mcpclient"
	"github.com/bigneek/picoflare/pkg/storage"
)

//go:embed workers/fib3d/index.js
var fib3dWorkerJS string

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

	// Help
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		printHelp()
		return
	}

	switch cmd {
	case "", "agent":
		runAgent(accountID, apiToken, r2AccessKey, r2SecretKey)
		return
	case "bot":
		workspace, _ := os.Getwd()
		runBot(bot.Config{
			TelegramToken:  telegramToken,
			AccountID:      accountID,
			APIToken:       apiToken,
			R2AccessKey:    r2AccessKey,
			R2SecretKey:    r2SecretKey,
			R2Bucket:       "pico-flare",
			VectorizeIndex: "picoflare-memory",
			LLMAPIKey:      os.Getenv("OPENROUTER_API_KEY"),
			LLMModel:       os.Getenv("OPENROUTER_MODEL"),
			Workspace:      workspace,
		})
		return
	case "mcp-test":
		runMCPTest(accountID, apiToken)
		return
	case "deploy-fib3d":
		if accountID == "" || apiToken == "" {
			log.Fatal("CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN required for deploy-fib3d")
		}
		ctx := context.Background()
		client := cf.NewClient(accountID, apiToken)
		if err := client.DeployWorker(ctx, "fib3d", fib3dWorkerJS); err != nil {
			log.Fatalf("Deploy fib3d failed: %v", err)
		}
		url := client.GetWorkerURL(ctx, "fib3d")
		fmt.Printf("fib3d deployed: %s\n", url)
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %q\n", cmd)
		printHelp()
		os.Exit(1)
	}
}

func printHelp() {
	fmt.Print(`pico-flare agent — Cloudflare-native AI agent (MCP + R2 + Vectorize)

Usage:
  picoflare              Run pico-flare agent (interactive; default)
  picoflare agent        Run pico-flare agent (interactive)
  picoflare bot          Telegram bot (TELEGRAM_BOT_TOKEN required)
  picoflare mcp-test     Create R2 bucket + Vectorize index via MCP
  picoflare deploy-fib3d Deploy fib3d Worker
  picoflare help         Show this help

When the MCP server is unavailable, the agent falls back to the Cloudflare
REST API so you still get Workers, R2, KV, D1, and Vectorize tools.

Set CLOUDFLARE_ACCOUNT_ID, CLOUDFLARE_API_TOKEN, OPENROUTER_API_KEY in .env.
`)
}

func runAgent(accountID, apiToken, r2AccessKey, r2SecretKey string) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Try MCP; on failure fall back to Cloudflare REST API (cfClient)
	var mcp *mcpclient.Client
	if accountID != "" && apiToken != "" {
		mcp = mcpclient.NewClient("https://mcp.cloudflare.com/mcp", apiToken, accountID)
		if err := mcp.Initialize(ctx); err != nil {
			log.Printf("MCP unavailable (%v), using Cloudflare REST API for Cloudflare operations", err)
			mcp = nil
		} else {
			log.Printf("pico-flare agent: MCP connected")
		}
	}

	var cfClient *cf.Client
	if accountID != "" && apiToken != "" {
		candidate := cf.NewClient(accountID, apiToken)
		if _, err := candidate.VerifyToken(ctx); err == nil {
			cfClient = candidate
			if mcp == nil {
				log.Printf("pico-flare agent: using Cloudflare REST API")
			}
		} else if mcp == nil {
			log.Printf("Cloudflare API token invalid; Cloudflare features limited. Set CLOUDFLARE_ACCOUNT_ID and CLOUDFLARE_API_TOKEN for full access.")
		}
	}

	var r2 *storage.R2Client
	if accountID != "" && r2AccessKey != "" && r2SecretKey != "" {
		r2Client, err := storage.NewR2Client(accountID, r2AccessKey, r2SecretKey)
		if err != nil {
			log.Printf("R2 client init failed (non-fatal): %v", err)
		} else {
			r2 = r2Client
		}
	}

	llmAPIKey := os.Getenv("OPENROUTER_API_KEY")
	llmModel := os.Getenv("OPENROUTER_MODEL")
	if llmModel == "" {
		llmModel = "anthropic/claude-3-5-sonnet"
	}
	var llmClient *llm.Client
	if llmAPIKey != "" {
		llmClient = llm.NewClient(llmAPIKey, llmModel)
		log.Printf("pico-flare agent: LLM %s", llmClient.Model)
	} else {
		log.Fatal("OPENROUTER_API_KEY is required for pico-flare agent. Set it in .env.")
	}

	workspace, _ := os.Getwd()
	ag := agent.New(agent.Config{
		LLM:                llmClient,
		MCP:                mcp,
		R2:                 r2,
		CF:                 cfClient,
		Bucket:             "pico-flare",
		AccountID:          accountID,
		Workspace:          workspace,
		OnSubagentComplete: nil,
	})

	fmt.Println("pico-flare agent — Interactive mode (Ctrl+C to exit)")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		reply := ag.ProcessMessage(ctx, 0, line)
		fmt.Println(reply)
		fmt.Println()
	}
	if err := scanner.Err(); err != nil {
		log.Printf("pico-flare agent: read error: %v", err)
	}
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
