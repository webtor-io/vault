package services

import (
	"context"
	"time"

	pg "github.com/go-pg/pg/v10"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	cs "github.com/webtor-io/common-services"
)

const (
	metricsRefreshIntervalFlag = "metrics-refresh-interval"
	metricsNamespace           = "vault"
)

// RegisterMetricsFlags registers CLI flags for the metrics refresher.
func RegisterMetricsFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.DurationFlag{
			Name:   metricsRefreshIntervalFlag,
			Usage:  "how often to recompute vault metrics from the database",
			Value:  60 * time.Second,
			EnvVar: "METRICS_REFRESH_INTERVAL",
		},
	)
}

// Metrics is a cs.Servable that periodically reads aggregate state from
// the vault database and publishes it as Prometheus metrics on the
// default registry. The HTTP endpoint is served by cs.Prom; we only
// keep gauges fresh here.
//
// Variant A (periodic refresh) was chosen over per-scrape collection so
// a scrape storm never multiplies DB load.
type Metrics struct {
	ctx      context.Context
	cancel   context.CancelFunc
	pg       *cs.PG
	interval time.Duration

	resourceCount      *prometheus.GaugeVec
	resourceBytes      *prometheus.GaugeVec
	resourceStored     *prometheus.GaugeVec
	fileCount          *prometheus.GaugeVec
	fileBytes          *prometheus.GaugeVec
	fileStored         *prometheus.GaugeVec
	orphanCount        prometheus.Gauge
	orphanBytes        prometheus.Gauge
	multipartInFlight  prometheus.Gauge
	leaseHeld          prometheus.Gauge
	refreshErrors      prometheus.Counter
	lastRefreshSuccess prometheus.Gauge
	refreshDuration    prometheus.Histogram
}

// NewMetrics constructs the Metrics service and registers its collectors
// on the default Prometheus registry.
func NewMetrics(c *cli.Context, pg *cs.PG) *Metrics {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Metrics{
		ctx:      ctx,
		cancel:   cancel,
		pg:       pg,
		interval: c.Duration(metricsRefreshIntervalFlag),

		resourceCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "resources_total",
			Help:      "Number of vault resources grouped by status.",
		}, []string{"status"}),
		resourceBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "resources_bytes_total",
			Help:      "Total logical size (resource.total_size) of vault resources by status.",
		}, []string{"status"}),
		resourceStored: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "resources_stored_bytes_total",
			Help:      "Bytes already uploaded to S3 (resource.stored_size) by status — storing label shows live progress.",
		}, []string{"status"}),

		fileCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "files_total",
			Help:      "Number of file rows in the dedup table by status.",
		}, []string{"status"}),
		fileBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "files_bytes_total",
			Help:      "Total logical size (file.total_size) of file rows by status — stored label is the real S3 footprint.",
		}, []string{"status"}),
		fileStored: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "files_stored_bytes_total",
			Help:      "Bytes uploaded per file row by status (file.stored_size).",
		}, []string{"status"}),

		orphanCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "orphan_files_total",
			Help:      "File rows with no resource_file reference — candidates for the gc sweep.",
		}),
		orphanBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "orphan_bytes_total",
			Help:      "Sum of total_size for orphan file rows.",
		}),
		multipartInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "multipart_uploads_in_flight",
			Help:      "File rows with a non-empty upload_id (open S3 multipart uploads).",
		}),
		leaseHeld: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "worker_leases_held",
			Help:      "Number of resources currently claimed by a worker (claim_expires_at > now).",
		}),
		refreshErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "metrics_refresh_errors_total",
			Help:      "Total number of failed metric refresh cycles.",
		}),
		lastRefreshSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "metrics_last_refresh_timestamp_seconds",
			Help:      "Unix timestamp of the last successful metrics refresh.",
		}),
		refreshDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "metrics_refresh_duration_seconds",
			Help:      "Wall-clock duration of a metrics refresh cycle.",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 8),
		}),
	}
	prometheus.MustRegister(
		m.resourceCount, m.resourceBytes, m.resourceStored,
		m.fileCount, m.fileBytes, m.fileStored,
		m.orphanCount, m.orphanBytes,
		m.multipartInFlight, m.leaseHeld,
		m.refreshErrors, m.lastRefreshSuccess, m.refreshDuration,
	)
	return m
}

// Serve runs the refresh loop until Close is called.
func (m *Metrics) Serve() error {
	log.WithField("interval", m.interval).Info("starting vault metrics refresher")

	// Populate metrics immediately so a fresh pod exposes meaningful
	// values on its first scrape rather than zeroes.
	m.refresh()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return nil
		case <-ticker.C:
			m.refresh()
		}
	}
}

// Close cancels the refresh loop.
func (m *Metrics) Close() {
	m.cancel()
}

func (m *Metrics) refresh() {
	start := time.Now()
	defer func() {
		m.refreshDuration.Observe(time.Since(start).Seconds())
	}()
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	if err := m.doRefresh(ctx); err != nil {
		m.refreshErrors.Inc()
		log.WithError(err).Warn("vault metrics refresh failed")
		return
	}
	m.lastRefreshSuccess.SetToCurrentTime()
}

type statusAggregate struct {
	Status      int16 `pg:"status"`
	Count       int64 `pg:"count"`
	TotalBytes  int64 `pg:"total_bytes"`
	StoredBytes int64 `pg:"stored_bytes"`
}

func (m *Metrics) doRefresh(ctx context.Context) error {
	db := m.pg.Get()
	if db == nil {
		return errors.New("db is nil")
	}

	// Resources grouped by status.
	var resourceStats []statusAggregate
	if _, err := db.QueryContext(ctx, &resourceStats, `
		SELECT status,
		       count(*)                           AS count,
		       COALESCE(sum(total_size),  0)::bigint AS total_bytes,
		       COALESCE(sum(stored_size), 0)::bigint AS stored_bytes
		FROM resource
		GROUP BY status
	`); err != nil {
		return errors.Wrap(err, "resource aggregate query failed")
	}
	m.resourceCount.Reset()
	m.resourceBytes.Reset()
	m.resourceStored.Reset()
	for _, s := range resourceStats {
		label := Status(s.Status).String()
		m.resourceCount.WithLabelValues(label).Set(float64(s.Count))
		m.resourceBytes.WithLabelValues(label).Set(float64(s.TotalBytes))
		m.resourceStored.WithLabelValues(label).Set(float64(s.StoredBytes))
	}

	// Files grouped by status.
	var fileStats []statusAggregate
	if _, err := db.QueryContext(ctx, &fileStats, `
		SELECT status,
		       count(*)                           AS count,
		       COALESCE(sum(total_size),  0)::bigint AS total_bytes,
		       COALESCE(sum(stored_size), 0)::bigint AS stored_bytes
		FROM file
		GROUP BY status
	`); err != nil {
		return errors.Wrap(err, "file aggregate query failed")
	}
	m.fileCount.Reset()
	m.fileBytes.Reset()
	m.fileStored.Reset()
	for _, s := range fileStats {
		label := Status(s.Status).String()
		m.fileCount.WithLabelValues(label).Set(float64(s.Count))
		m.fileBytes.WithLabelValues(label).Set(float64(s.TotalBytes))
		m.fileStored.WithLabelValues(label).Set(float64(s.StoredBytes))
	}

	// Orphan files (no resource_file).
	var orphan struct {
		Count int64 `pg:"count"`
		Bytes int64 `pg:"bytes"`
	}
	if _, err := db.QueryOneContext(ctx, &orphan, `
		SELECT count(*)                               AS count,
		       COALESCE(sum(f.total_size), 0)::bigint AS bytes
		FROM file f
		WHERE NOT EXISTS (
			SELECT 1 FROM resource_file rf WHERE rf.file_hash = f.hash
		)
	`); err != nil {
		return errors.Wrap(err, "orphan aggregate query failed")
	}
	m.orphanCount.Set(float64(orphan.Count))
	m.orphanBytes.Set(float64(orphan.Bytes))

	// Open multipart uploads (file rows with non-empty upload_id).
	var mpCount int64
	if _, err := db.QueryOneContext(ctx, pg.Scan(&mpCount), `
		SELECT count(*) FROM file WHERE upload_id IS NOT NULL AND upload_id <> ''
	`); err != nil {
		return errors.Wrap(err, "multipart count query failed")
	}
	m.multipartInFlight.Set(float64(mpCount))

	// Active worker leases.
	var leaseCount int64
	if _, err := db.QueryOneContext(ctx, pg.Scan(&leaseCount), `
		SELECT count(*) FROM resource WHERE claim_expires_at IS NOT NULL AND claim_expires_at > now()
	`); err != nil {
		return errors.Wrap(err, "lease count query failed")
	}
	m.leaseHeld.Set(float64(leaseCount))

	return nil
}
