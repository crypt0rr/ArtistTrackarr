-- Hot scheduled and administrative queries. These additive indexes keep
-- release-day date windows and delivery-audit ordering bounded as history
-- grows; no rows are rewritten or removed.
CREATE INDEX IF NOT EXISTS release_groups_precision_date
  ON release_groups(date_precision, first_release_date);
CREATE INDEX IF NOT EXISTS notification_events_created_id
  ON notification_events(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS deliveries_event_id
  ON deliveries(event_id, id);
