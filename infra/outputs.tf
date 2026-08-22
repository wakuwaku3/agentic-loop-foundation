output "cloud_run_uri" {
  value       = google_cloud_run_v2_service.app.uri
  description = "Direct Cloud Run URI protected by IAP."
}

output "runtime_service_account" {
  value = google_service_account.runtime.email
}

output "image" {
  value = local.image
}
