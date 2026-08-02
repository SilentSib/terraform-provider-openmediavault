# Import an existing SSL certificate using its OMV-assigned UUID.
# private_key_pem will not be populated by the import (OMV's read API
# never returns it) -- set it explicitly afterward if you want Terraform
# to manage it.
terraform import omv_ssl_certificate.this 5f2f4e9e-1234-4a5b-8abc-1234567890ab
