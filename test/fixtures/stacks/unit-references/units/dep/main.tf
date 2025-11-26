variable "message" {
  description = "Input message"
  type        = string
}

resource "local_file" "file" {
  content  = var.message
  filename = "${path.module}/data.txt"
}

output "result" {
  value = var.message
}

