// cpa-billing-verify performs read-only, offline parity checks on old CPA data.
// It never opens an upstream connection or prints request/credential contents.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chiphalo/billing"
)

func run() error {
	usagePath := flag.String("usage", "", "Existing usage JSONL (read-only)")
	guiPath := flag.String("gui", "", "Existing GUI price configuration (read-only)")
	accountsPath := flag.String("accounts", "", "Optional public quota snapshot (read-only)")
	modelsPath := flag.String("models", "", "Optional exposed OAuth catalog (read-only)")
	resetsPath := flag.String("reset-marks", "", "Optional existing billing reset timestamps (read-only)")
	asOf := flag.Int64("as-of", 0, "Fixed epoch for reproducible period checks")
	flag.Parse()
	if *usagePath == "" || *guiPath == "" {
		return fmt.Errorf("usage and gui paths are required")
	}
	raw, err := os.ReadFile(*guiPath)
	if err != nil {
		return err
	}
	prices, err := billing.LoadPrices(raw)
	if err != nil {
		return err
	}
	var acc *billing.Accumulator
	if *accountsPath != "" {
		if *asOf <= 0 || *modelsPath == "" {
			return fmt.Errorf("period verification requires as-of and models")
		}
		var accounts struct {
			Accounts []billing.Account `json:"accounts"`
		}
		data, errRead := os.ReadFile(*accountsPath)
		if errRead != nil {
			return errRead
		}
		if err := json.Unmarshal(data, &accounts); err != nil {
			return err
		}
		modelData, errRead := os.ReadFile(*modelsPath)
		if errRead != nil {
			return errRead
		}
		var root struct {
			Models []json.RawMessage `json:"models"`
		}
		var models []json.RawMessage
		if err := json.Unmarshal(modelData, &models); err != nil {
			if err := json.Unmarshal(modelData, &root); err != nil {
				return err
			}
			models = root.Models
		}
		known := make(map[string]bool)
		for _, item := range models {
			var id string
			if err := json.Unmarshal(item, &id); err != nil {
				var object struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(item, &object)
				id = object.ID
			}
			if id = strings.TrimPrefix(strings.TrimSpace(id), "codex/"); id != "" {
				known[id] = true
			}
		}
		marks := make(map[string]int64)
		if *resetsPath != "" {
			data, errRead := os.ReadFile(*resetsPath)
			if errRead != nil && !os.IsNotExist(errRead) {
				return errRead
			}
			if errRead == nil {
				if err := json.Unmarshal(data, &marks); err != nil {
					return err
				}
			}
		}
		acc = billing.NewAccumulator(accounts.Accounts, known, prices, *asOf, marks)
	}
	file, err := os.Open(*usagePath)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	hash := sha256.New()
	count, skipped, sum := 0, 0, float64(0)
	for scanner.Scan() {
		row, errDecode := billing.DecodeRow(scanner.Bytes())
		if errDecode != nil {
			skipped++
			continue
		}
		cost := billing.Cost(row, prices)
		_, _ = fmt.Fprintf(hash, "%d:%.8f\n", count, cost)
		sum = float64(sum + cost)
		count++
		if acc != nil {
			acc.Add(row)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	result := map[string]any{"version": billing.Version, "rows": count, "skipped": skipped,
		"configured_prices": len(prices), "cost_usd": billing.RoundDecimal(sum, 8),
		"receipt_sha256": hex.EncodeToString(hash.Sum(nil))}
	if acc != nil {
		result["cycle_summary_sha256"] = billing.SummaryDigest(acc.Summaries())
		result["as_of"] = *asOf
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "billing verification failed:", err)
		os.Exit(1)
	}
}
