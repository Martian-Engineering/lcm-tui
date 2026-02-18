package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type dissolveOptions struct {
	summaryID string
	apply     bool
	purge     bool // delete the condensed summary record too
}

type dissolveTarget struct {
	summaryID      string
	conversationID int64
	kind           string
	depth          int
	tokenCount     int
	ordinal        int64
}

type dissolveParent struct {
	summaryID  string
	ordinal    int // position within summary_parents
	kind       string
	depth      int
	tokenCount int
	content    string
}

// runDissolveCommand executes the standalone dissolve CLI path.
func runDissolveCommand(args []string) error {
	opts, conversationID, err := parseDissolveArgs(args)
	if err != nil {
		return err
	}

	paths, err := resolveDataPaths()
	if err != nil {
		return err
	}

	db, err := openLCMDB(paths.lcmDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()

	// Load the target condensed summary
	target, err := loadDissolveTarget(ctx, db, conversationID, opts.summaryID)
	if err != nil {
		return err
	}

	// Load its parent summaries in order
	parents, err := loadDissolveParents(ctx, db, opts.summaryID)
	if err != nil {
		return err
	}
	if len(parents) == 0 {
		return fmt.Errorf("summary %s has no parent summaries — nothing to dissolve", opts.summaryID)
	}

	// Show plan
	fmt.Printf("Dissolve %s (%s, d%d, %dt) at context ordinal %d\n",
		target.summaryID, target.kind, target.depth, target.tokenCount, target.ordinal)
	fmt.Printf("Restore %d parent summaries:\n", len(parents))

	totalParentTokens := 0
	for _, p := range parents {
		preview := oneLine(p.content)
		preview = truncateString(preview, 80)
		fmt.Printf("  [%d] %s (%s, d%d, %dt) %s\n", p.ordinal, p.summaryID, p.kind, p.depth, p.tokenCount, preview)
		totalParentTokens += p.tokenCount
	}
	fmt.Printf("\nToken impact: %dt condensed → %dt restored (%+dt)\n",
		target.tokenCount, totalParentTokens, totalParentTokens-target.tokenCount)

	// Count items that will shift
	var totalItems int
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM context_items
		WHERE conversation_id = ? AND ordinal > ?
	`, conversationID, target.ordinal).Scan(&totalItems)
	if err != nil {
		return fmt.Errorf("count items to shift: %w", err)
	}
	shift := len(parents) - 1
	fmt.Printf("Ordinal shift: %d items after ordinal %d will shift by +%d\n", totalItems, target.ordinal, shift)

	if !opts.apply {
		fmt.Println("\nDry run. Use --apply to execute.")
		return nil
	}

	// Execute in transaction
	fmt.Println("\nApplying...")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	// Step 1: Delete the condensed summary's context_item
	res, err := tx.ExecContext(ctx, `
		DELETE FROM context_items
		WHERE conversation_id = ? AND ordinal = ? AND summary_id = ?
	`, conversationID, target.ordinal, target.summaryID)
	if err != nil {
		return fmt.Errorf("delete condensed context_item: %w", err)
	}
	deleted, _ := res.RowsAffected()
	if deleted != 1 {
		return fmt.Errorf("expected to delete 1 context_item, deleted %d", deleted)
	}
	fmt.Printf("  ✓ Deleted context_item at ordinal %d\n", target.ordinal)

	// Step 2: Shift items after the removed ordinal up by (parentCount - 1)
	// Use a two-phase approach to avoid PRIMARY KEY conflicts:
	// Phase A: move to temporary high ordinals
	// Phase B: set final ordinals
	if shift > 0 {
		const tempOffset = 10_000_000
		_, err = tx.ExecContext(ctx, `
			UPDATE context_items
			SET ordinal = ordinal + ?
			WHERE conversation_id = ? AND ordinal > ?
		`, tempOffset, conversationID, target.ordinal)
		if err != nil {
			return fmt.Errorf("shift items to temp ordinals: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE context_items
			SET ordinal = ordinal - ? + ?
			WHERE conversation_id = ? AND ordinal >= ?
		`, tempOffset, shift, conversationID, tempOffset)
		if err != nil {
			return fmt.Errorf("shift items to final ordinals: %w", err)
		}
		fmt.Printf("  ✓ Shifted %d items by +%d ordinals\n", totalItems, shift)
	}

	// Step 3: Insert parent summaries at ordinals [target.ordinal, target.ordinal + len(parents) - 1]
	for i, p := range parents {
		newOrdinal := target.ordinal + int64(i)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO context_items (conversation_id, ordinal, item_type, summary_id, created_at)
			VALUES (?, ?, 'summary', ?, datetime('now'))
		`, conversationID, newOrdinal, p.summaryID)
		if err != nil {
			return fmt.Errorf("insert parent %s at ordinal %d: %w", p.summaryID, newOrdinal, err)
		}
	}
	fmt.Printf("  ✓ Inserted %d parent summaries at ordinals %d–%d\n",
		len(parents), target.ordinal, target.ordinal+int64(len(parents)-1))

	// Step 4: Optionally purge the condensed summary record
	if opts.purge {
		// Remove parent links first
		_, err = tx.ExecContext(ctx, `
			DELETE FROM summary_parents WHERE summary_id = ?
		`, target.summaryID)
		if err != nil {
			return fmt.Errorf("delete summary_parents for %s: %w", target.summaryID, err)
		}
		_, err = tx.ExecContext(ctx, `
			DELETE FROM summaries WHERE summary_id = ?
		`, target.summaryID)
		if err != nil {
			return fmt.Errorf("delete summary record %s: %w", target.summaryID, err)
		}
		fmt.Printf("  ✓ Purged summary record %s\n", target.summaryID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	rollback = false

	// Verify
	var newCount int
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM context_items WHERE conversation_id = ?
	`, conversationID).Scan(&newCount)
	fmt.Printf("\nDone. Context now has %d items. Changes take effect on next conversation turn.\n", newCount)
	return nil
}

func parseDissolveArgs(args []string) (dissolveOptions, int64, error) {
	fs := flag.NewFlagSet("dissolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	summaryID := fs.String("summary-id", "", "summary ID to dissolve (required)")
	apply := fs.Bool("apply", false, "apply changes to the DB")
	purge := fs.Bool("purge", false, "also delete the condensed summary record")

	// Normalize: pull positional args out so flags parse correctly regardless of order
	normalized, err := normalizeDissolveArgs(args)
	if err != nil {
		return dissolveOptions{}, 0, fmt.Errorf("%w\n%s", err, dissolveUsageText())
	}
	if err := fs.Parse(normalized); err != nil {
		return dissolveOptions{}, 0, fmt.Errorf("%w\n%s", err, dissolveUsageText())
	}

	if strings.TrimSpace(*summaryID) == "" {
		return dissolveOptions{}, 0, fmt.Errorf("--summary-id is required\n%s", dissolveUsageText())
	}

	if fs.NArg() != 1 {
		return dissolveOptions{}, 0, fmt.Errorf("conversation ID is required\n%s", dissolveUsageText())
	}

	conversationID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		return dissolveOptions{}, 0, fmt.Errorf("parse conversation ID %q: %w\n%s", fs.Arg(0), err, dissolveUsageText())
	}

	return dissolveOptions{
		summaryID: strings.TrimSpace(*summaryID),
		apply:     *apply,
		purge:     *purge,
	}, conversationID, nil
}

func normalizeDissolveArgs(args []string) ([]string, error) {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--apply" || arg == "--purge":
			flags = append(flags, arg)
		case strings.HasPrefix(arg, "--summary-id="):
			flags = append(flags, arg)
		case arg == "--summary-id":
			if i+1 >= len(args) {
				return nil, errors.New("missing value for --summary-id")
			}
			flags = append(flags, arg, args[i+1])
			i++
		case strings.HasPrefix(arg, "--"):
			flags = append(flags, arg)
		default:
			positionals = append(positionals, arg)
		}
	}
	return append(flags, positionals...), nil
}

func dissolveUsageText() string {
	return strings.TrimSpace(`
Usage:
  lcm-tui dissolve <conversation_id> --summary-id <id> [--apply] [--purge]

Dissolve a condensed summary back into its constituent parent summaries
in the active context. Restores the parents as individual context_items
at the position the condensed node occupied.

Flags:
  --summary-id <id>   Condensed summary to dissolve (required)
  --apply             Execute changes (default: dry run)
  --purge             Also delete the condensed summary record from DB
`)
}

func loadDissolveTarget(ctx context.Context, db *sql.DB, conversationID int64, summaryID string) (dissolveTarget, error) {
	var target dissolveTarget
	err := db.QueryRowContext(ctx, `
		SELECT
			s.summary_id,
			s.conversation_id,
			s.kind,
			s.depth,
			s.token_count,
			ci.ordinal
		FROM summaries s
		JOIN context_items ci
			ON ci.summary_id = s.summary_id
			AND ci.conversation_id = s.conversation_id
		WHERE s.summary_id = ?
		  AND s.conversation_id = ?
	`, summaryID, conversationID).Scan(
		&target.summaryID,
		&target.conversationID,
		&target.kind,
		&target.depth,
		&target.tokenCount,
		&target.ordinal,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return dissolveTarget{}, fmt.Errorf("summary %s not found in active context for conversation %d", summaryID, conversationID)
	}
	if err != nil {
		return dissolveTarget{}, fmt.Errorf("load dissolve target: %w", err)
	}
	if target.kind != "condensed" {
		return dissolveTarget{}, fmt.Errorf("summary %s is a %s (depth %d), not condensed — only condensed summaries can be dissolved", summaryID, target.kind, target.depth)
	}
	return target, nil
}

func loadDissolveParents(ctx context.Context, db *sql.DB, summaryID string) ([]dissolveParent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT
			sp.parent_summary_id,
			sp.ordinal,
			s.kind,
			s.depth,
			s.token_count,
			s.content
		FROM summary_parents sp
		JOIN summaries s ON s.summary_id = sp.parent_summary_id
		WHERE sp.summary_id = ?
		ORDER BY sp.ordinal ASC
	`, summaryID)
	if err != nil {
		return nil, fmt.Errorf("query parents for %s: %w", summaryID, err)
	}
	defer rows.Close()

	var parents []dissolveParent
	for rows.Next() {
		var p dissolveParent
		if err := rows.Scan(&p.summaryID, &p.ordinal, &p.kind, &p.depth, &p.tokenCount, &p.content); err != nil {
			return nil, fmt.Errorf("scan parent row: %w", err)
		}
		parents = append(parents, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate parents: %w", err)
	}
	return parents, nil
}
