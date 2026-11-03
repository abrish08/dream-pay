package metrics

import (
	"context"

	"gorm.io/gorm"
)

// SQLGauge registers a gauge read from a query that returns the label
// columns followed by a numeric column n, e.g.
//
//	SELECT status, COUNT(*) AS n FROM deliveries GROUP BY status
func SQLGauge(name, help string, fixed map[string]string, db *gorm.DB, query string, labelColumns ...string) {
	Gauge(name, help, fixed, func(ctx context.Context) ([]Sample, error) {
		rows, err := db.WithContext(ctx).Raw(query).Rows()
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []Sample
		for rows.Next() {
			vals := make([]string, len(labelColumns))
			var n float64
			dest := make([]any, 0, len(vals)+1)
			for i := range vals {
				dest = append(dest, &vals[i])
			}
			if err := rows.Scan(append(dest, &n)...); err != nil {
				return nil, err
			}
			labels := map[string]string{}
			for i, c := range labelColumns {
				labels[c] = vals[i]
			}
			out = append(out, Sample{Labels: labels, Value: n})
		}
		return out, rows.Err()
	})
}
