---
title: "Weiyun"
description: "Rclone support for Tencent Weiyun"
---

# {{< icon "fa fa-cloud" >}} Weiyun

This is an experimental backend for Tencent Weiyun.

Configuration options:

- `cookie`: Login cookie used to authenticate with Weiyun.
- `base_url`: API endpoint, defaults to `https://api.weiyun.com`.
- `user_agent`: Custom User-Agent header sent to the API.

Example:

```bash
rclone config create wy weiyun cookie "<your_cookie>"
rclone ls wy:/
```
