// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package sqlite

import (
	"strings"
	"testing"
)

// The delivery-id index is partial, so a query only uses it if it says
// "delivery_id IS NOT NULL". Without that term SQLite scanned a million rows.
// This asserts the plan, which is cheap, so a regression fails here instead of
// showing up as a slow production query.
func TestDeliveryIDQueryUsesTheIndex(t *testing.T) {
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for name, stmt := range map[string]string{
		"Query by delivery_id": queryDeliveryIDPlanSQL,
	} {
		rows, err := s.rdb.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+stmt, 1, "evt_1", 50)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		joined := strings.Join(plan, " | ")
		if !strings.Contains(joined, "messages_inbox_delivery") && !strings.Contains(joined, "messages_dedupe") {
			t.Errorf("%s does not use a delivery_id index: %s", name, joined)
		}
		if strings.Contains(joined, "SCAN messages") && !strings.Contains(joined, "USING") {
			t.Errorf("%s scans the whole table: %s", name, joined)
		}
	}
}
