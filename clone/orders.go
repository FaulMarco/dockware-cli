package clone

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// orderParents are the tables whose kept rows other order tables point to,
// via a <table>_id column (checked after the generic order_id rule).
var orderParents = []string{"order_delivery", "order_line_item", "order_transaction"}

// orderHistoryEntities are the state_machine_history entity names whose
// referenced rows are windowed; all other history rows are kept.
var orderHistoryEntities = []string{"order", "order_delivery", "order_transaction"}

// OrderWindowWhere restricts the export to orders placed within the last days
// before now and to the rows that hang off them (addresses, line items,
// deliveries, transactions, captures, refunds, documents, tags, state history
// and any extension table with a binary order_id), so a shop with years of
// history clones in minutes. The cutoff is one literal (midnight UTC, days
// days ago) embedded in every condition: the queries of the child tables run
// after the order query, and a cutoff evaluated per query would keep orders
// without their rows. Every version row of a kept order is kept, as the ids
// are compared without version. Conditions use unqualified table names, which
// both strategies run with the live database as default schema. Tables or
// columns missing from the probe are left unfiltered; days <= 0 or a schema
// without order.order_date_time means no window (nil).
func OrderWindowWhere(p *Probe, days int, now time.Time) map[string]string {
	if days <= 0 {
		return nil
	}
	return orderWindowWhere(p, "'"+OrderCutoff(days, now)+"'")
}

// OrderCutoff is the first order date the window keeps: midnight UTC, days
// days before now.
func OrderCutoff(days int, now time.Time) string {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -days).Format("2006-01-02 15:04:05")
}

// applyOrderWindow restricts plan to the orders of the last days before now
// and returns the rules hash of the resulting plan and the cutoff; cutoff is
// empty when no window applies (days <= 0 or no order date on the source).
// The hash names the window by its days instead of the cutoff date, so a
// dump kept with --keep-dump stays reusable on the next day.
func applyOrderWindow(plan *DumpPlan, p *Probe, days int, now time.Time) (hash, cutoff string) {
	hashed := DumpPlan{Where: map[string]string{}}
	for t, cond := range plan.Where {
		hashed.Where[t] = cond
	}
	if where := OrderWindowWhere(p, days, now); where != nil {
		cutoff = OrderCutoff(days, now)
		marker := orderWindowWhere(p, fmt.Sprintf("orders_since_days=%d", days))
		for _, t := range sortedKeys(where) {
			plan.AddWhere(t, where[t])
			hashed.AddWhere(t, marker[t])
		}
	}
	return RulesHash(plan.Rewrites, plan.SchemaOnly, hashed.Where), cutoff
}

// orderWindowWhere builds the window conditions with cutoff as the SQL
// literal the order date is compared against.
func orderWindowWhere(p *Probe, cutoff string) map[string]string {
	if p.Schema["order"]["order_date_time"].Name == "" {
		return nil
	}
	where := map[string]string{
		"order": "order_date_time >= " + cutoff,
	}
	kept := func(table string) string {
		return "(SELECT `" + table + "`.`id` FROM `" + table + "` WHERE " + where[table] + ")"
	}
	// ref restricts table to rows whose binary column points into the kept
	// rows of parent; NULL references (optional links) are kept.
	ref := func(table, column, parent string) {
		col, ok := p.Schema[table][column]
		if !ok || where[table] != "" || where[parent] == "" || !isBinary(col.DataType) {
			return
		}
		cond := "`" + column + "` IN " + kept(parent)
		if col.Nullable {
			cond = "(`" + column + "` IS NULL OR " + cond + ")"
		}
		where[table] = cond
	}
	tables := make([]string, 0, len(p.Schema))
	for t := range p.Schema {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		if t != "order" {
			ref(t, "order_id", "order")
		}
	}
	ref("order_transaction_capture", "order_transaction_id", "order_transaction")
	ref("order_transaction_capture_refund", "capture_id", "order_transaction_capture")
	ref("order_transaction_capture_refund_position", "refund_id", "order_transaction_capture_refund")
	for _, t := range tables {
		for _, parent := range orderParents {
			ref(t, parent+"_id", parent)
		}
	}

	// Shopware 6.5.7+ stores the referenced row in referenced_id; older
	// schemas only have the JSON entity_id and keep their full history.
	hist := p.Schema["state_machine_history"]
	if hist["entity_name"].Name != "" && isBinary(hist["referenced_id"].DataType) {
		var names, branches []string
		for _, t := range orderHistoryEntities {
			if where[t] != "" {
				names = append(names, "'"+t+"'")
				branches = append(branches, "(`entity_name` = '"+t+"' AND `referenced_id` IN "+kept(t)+")")
			}
		}
		where["state_machine_history"] = "`entity_name` NOT IN (" + strings.Join(names, ",") + ") OR " + strings.Join(branches, " OR ")
	}
	return where
}

func isBinary(dataType string) bool { return dataType == "binary" || dataType == "varbinary" }
