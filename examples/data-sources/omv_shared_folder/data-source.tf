data "omv_shared_folder" "media" {
  name = "media"
}

output "media_shared_folder_id" {
  value = data.omv_shared_folder.media.id
}
