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

resource "google_firestore_index" "expired_active_leases" {
  project    = var.project_id
  database   = google_firestore_database.default.name
  collection = "leases"

  fields {
    field_path = "lease_status"
    order      = "ASCENDING"
  }
  fields {
    field_path = "lease_expires_at"
    order      = "ASCENDING"
  }
  fields {
    field_path = "__name__"
    order      = "ASCENDING"
  }

  lifecycle {
    prevent_destroy = true
  }
}
