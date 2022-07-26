---
console:
  username: {{ .name }}
  password: {{ .name }}_password
  signing_key: {{ .name }}_signing
  max_message_size_bytes: 409600
logger:
  level: "DEBUG"
socket:
  server_key: {{ .name }}_server
  max_message_size_bytes: 4096 # reserved buffer
  max_request_size_bytes: 131072
session:
  token_expiry_sec: 7200 # 2 hours
  encryption_key: {{ .name }}_enc
  refresh_encryption_key: {{ .name }}_refresh
runtime:
  http_key: {{ .name }}
