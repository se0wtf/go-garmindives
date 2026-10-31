Swagger UI v5.34.1 is vendored from the official swagger-api/swagger-ui release:
https://github.com/swagger-api/swagger-ui/releases/tag/v5.34.1

Upstream files, kept unmodified:
- dist/swagger-ui-bundle.js
- dist/swagger-ui.css
- dist/swagger-ui-bundle.js.LICENSE.txt
- LICENSE

index.html and swagger-initializer.js configure the locally served API. There
are no runtime CDN dependencies or requests to Swagger's online validator.

To update the UI, replace the four upstream files with those from a pinned
release, update this version, and run the API documentation tests. Keep the
upstream license and bundled third-party notices with the distribution.
