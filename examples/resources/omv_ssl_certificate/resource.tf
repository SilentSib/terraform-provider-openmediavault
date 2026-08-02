# Bring your own certificate, e.g. generated with the community
# hashicorp/tls provider (self-signed here for simplicity -- swap in an
# ACME/Let's Encrypt or internally-issued certificate just as easily).
terraform {
  required_providers {
    omv = {
      source = "example/openmediavault"
    }
    tls = {
      source = "hashicorp/tls"
    }
  }
}

resource "tls_private_key" "this" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "tls_self_signed_cert" "this" {
  private_key_pem = tls_private_key.this.private_key_pem

  subject {
    common_name  = "nas.example.lan"
    organization = "Example Org"
  }

  validity_period_hours = 24 * 365
  allowed_uses = [
    "key_encipherment",
    "digital_signature",
    "server_auth",
  ]
}

resource "omv_ssl_certificate" "this" {
  certificate_pem = tls_self_signed_cert.this.cert_pem
  private_key_pem = tls_private_key.this.private_key_pem
  comment         = "Managed by Terraform"
}

# Use it, e.g. for the web UI itself:
resource "omv_workbench_settings" "this" {
  enable_ssl         = true
  ssl_certificate_id = omv_ssl_certificate.this.id
}
