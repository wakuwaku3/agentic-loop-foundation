output "cloud_run_uri" {
  value       = google_cloud_run_v2_service.app.uri
  description = "Direct Cloud Run URI protected by IAP."
}

output "runtime_service_account" {
  value = google_service_account.runtime.email
}

output "reconciler_service_account" {
  value       = google_service_account.reconciler.email
  description = "Dedicated identity for an optional authenticated reconcile trigger."
}

output "reconcile_scheduler_name" {
  value       = var.enable_reconcile_scheduler ? google_cloud_scheduler_job.reconcile[0].name : null
  description = "Optional Scheduler job; null when cost-gated scheduling is disabled."
}

output "image" {
  value = local.image
}
