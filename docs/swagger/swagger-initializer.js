window.addEventListener("load", function () {
  window.ui = SwaggerUIBundle({
    url: "../api/v1/openapi.json",
    dom_id: "#swagger-ui",
    presets: [SwaggerUIBundle.presets.apis],
    layout: "BaseLayout",
    deepLinking: true,
    displayOperationId: true,
    displayRequestDuration: true,
    filter: true,
    validatorUrl: null,
    queryConfigEnabled: false,
    supportedSubmitMethods: ["get", "head", "post", "patch", "delete"],
  });
});
