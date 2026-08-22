resource "google_firestore_index" "outbox_delivery" {
  project    = var.project_id
  database   = google_firestore_database.default.name
  collection = "outbox"

  fields {
    field_path = "outbox_status"
    order      = "ASCENDING"
  }
  fields {
    field_path = "outbox_next_attempt_at"
    order      = "ASCENDING"
  }

  lifecycle {
    prevent_destroy = true
  }
}
