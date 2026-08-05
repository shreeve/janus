# TODO

- [ ] Make HTTPS the only public transport: redirect HTTP to HTTPS in every
      environment, and emit `Strict-Transport-Security: max-age=31536000` on
      production HTTPS responses. Do not enable `includeSubDomains` or
      `preload`. Certify redirects and HSTS across Janus response classes.
