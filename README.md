# logger

how to use sigNoz
```
SERVICE_NAME=goApp INSECURE_MODE=true OTEL_EXPORTER_OTLP_ENDPOINT=<IP of SigNoz backend>:4317
```

```
library

"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
```
func main() {
	r := gin.Default()
    cleanup := logger.InitSigNozTracer()
	defer cleanup(context.Background())
	r.Use(logger.Middleware(serviceName))
    r.Use(otelgin.Middleware(serviceName))
}

Logger
```
sugarLogger := logger.ConfigZap()

sugarLogger.Infow("Get the time now with format","time",time.Now().Format("2006-January-02"))
sugarLogger.Infof("Today is :%s",time.Now().Format("2006-January-02"))
```