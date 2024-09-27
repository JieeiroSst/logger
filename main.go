package logger

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/snowflake"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/render"
	"github.com/go-redis/redis"
	consulapi "github.com/hashicorp/consul/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc/credentials"
	"gorm.io/gorm"
)

var userStatus = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_request_get_user_status_count",
		Help: "Count of status returned by user.",
	},
	[]string{"user", "status"},
)

func init() {
	prometheus.MustRegister(userStatus)
}

func Metrics() http.Handler {
	return promhttp.Handler()
}

type SigNoz struct {
	ServiceName              string
	OtelExporterOtlpEndpoint string
	InsecureMode             string
}

func InitSigNozTracer(sigNoz SigNoz) func(context.Context) error {
	secureOption := otlptracegrpc.WithTLSCredentials(credentials.NewClientTLSFromCert(nil, ""))
	if len(sigNoz.InsecureMode) > 0 {
		secureOption = otlptracegrpc.WithInsecure()
	}

	exporter, err := otlptrace.New(
		context.Background(),
		otlptracegrpc.NewClient(
			secureOption,
			otlptracegrpc.WithEndpoint(sigNoz.OtelExporterOtlpEndpoint),
		),
	)

	if err != nil {
		log.Fatal(err)
	}
	resources, err := resource.New(
		context.Background(),
		resource.WithAttributes(
			attribute.String("service.name", sigNoz.ServiceName),
			attribute.String("library.language", "go"),
		),
	)
	if err != nil {
		log.Printf("Could not set resources: ", err)
	}

	otel.SetTracerProvider(
		sdktrace.NewTracerProvider(
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(resources),
		),
	)
	return exporter.Shutdown
}

func ConfigZap() *zap.SugaredLogger {
	cfg := zap.Config{
		Encoding:    "json",
		Level:       zap.NewAtomicLevelAt(zap.InfoLevel),
		OutputPaths: []string{"stderr"},

		EncoderConfig: zapcore.EncoderConfig{
			MessageKey:   "message",
			TimeKey:      "time",
			LevelKey:     "level",
			CallerKey:    "caller",
			EncodeCaller: zapcore.FullCallerEncoder,
			EncodeLevel:  CustomLevelEncoder,
			EncodeTime:   SyslogTimeEncoder,
		},
	}

	logger, _ := cfg.Build()
	return logger.Sugar()
}

func SyslogTimeEncoder(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString(t.Format("2006-01-02 15:04:05"))
}

func CustomLevelEncoder(level zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	enc.AppendString("[" + level.CapitalString() + "]")
}

type MessageStatus struct {
	Message string      `json:"message,omitempty"`
	Error   bool        `json:"error"`
	Data    interface{} `json:"data,omitempty"`
}

func ResponseStatus(c *gin.Context, code int, response interface{}) {
	data, err := json.Marshal(response)
	if err != nil {
		panic(err)
	}
	c.Render(
		code, render.Data{
			ContentType: "application/json",
			Data:        data,
		})
}

func GearedIntID() int {
	n, err := snowflake.NewNode(1)
	if err != nil {
		return 0
	}
	id, err := strconv.Atoi(n.Generate().String())
	if err != nil {
		return 0
	}
	return id
}

func GearedStringID() string {
	n, err := snowflake.NewNode(1)
	if err != nil {
		return ""
	}
	return n.Generate().String()
}

func DecodeBase(msg, decode string) bool {
	msgDecode, err := base64.StdEncoding.DecodeString(msg)
	if err != nil {
		return false
	}
	if !strings.EqualFold(string(msgDecode), decode) {
		return false
	}
	return true
}

func DecodeByte(msg string) []byte {
	sDec, err := base64.StdEncoding.DecodeString(msg)
	if err != nil {
		return nil
	}
	return sDec
}

type Pagination struct {
	Limit      int         `json:"limit,omitempty" query:"limit"`
	Page       int         `json:"page,omitempty" query:"page"`
	Sort       string      `json:"sort,omitempty" query:"sort"`
	TotalRows  int64       `json:"total_rows"`
	TotalPages int         `json:"total_pages"`
	Rows       interface{} `json:"rows"`
}

func (p *Pagination) GetOffset() int {
	return (p.GetPage() - 1) * p.GetLimit()
}

func (p *Pagination) GetLimit() int {
	if p.Limit == 0 {
		p.Limit = 10
	}
	return p.Limit
}

func (p *Pagination) GetPage() int {
	if p.Page == 0 {
		p.Page = 1
	}
	return p.Page
}

func (p *Pagination) GetSort() string {
	if p.Sort == "" {
		p.Sort = "Id desc"
	}
	return p.Sort
}

func Paginate(value interface{}, pagination *Pagination, db *gorm.DB) func(db *gorm.DB) *gorm.DB {
	var totalRows int64
	db.Model(value).Count(&totalRows)

	pagination.TotalRows = totalRows
	totalPages := int(math.Ceil(float64(totalRows) / float64(pagination.Limit)))
	pagination.TotalPages = totalPages

	return func(db *gorm.DB) *gorm.DB {
		return db.Offset(pagination.GetOffset()).Limit(pagination.GetLimit()).Order(pagination.GetSort())
	}
}

type configConsul struct {
	Host    string
	Key     string
	Service string
}

type Consul interface {
	getEnv(key, fallback string) string
	getConsul(address string) (*consulapi.Client, error)
	getKvPair(client *consulapi.Client, key string) (*consulapi.KVPair, error)
	ConnectConfigConsul() (config []byte, err error)
}

func NewConfigConsul(host, key, service string) Consul {
	return &configConsul{
		Host:    host,
		Key:     key,
		Service: service,
	}
}

func (c *configConsul) getEnv(key, fallback string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		value = fallback
	}
	return value
}

func (c *configConsul) getConsul(address string) (*consulapi.Client, error) {
	config := consulapi.DefaultConfig()
	config.Address = address
	consul, err := consulapi.NewClient(config)
	return consul, err

}

func (c *configConsul) getKvPair(client *consulapi.Client, key string) (*consulapi.KVPair, error) {
	kv := client.KV()
	keyPair, _, err := kv.Get(key, nil)
	return keyPair, err
}

func (c *configConsul) ConnectConfigConsul() ([]byte, error) {
	consul, err := c.getConsul(c.Host)
	if err != nil {
		return nil, err
	}

	cat := consul.Catalog()
	_, _, err = cat.Service(c.Service, "", nil)
	if err != nil {
		return nil, err
	}

	redisPattern, err := c.getKvPair(consul, c.Key)
	if err != nil {
		return nil, err
	}

	if redisPattern == nil {
		return nil, nil
	}

	return redisPattern.Value, nil
}

type ClientCircuitBreakerProxy struct {
	logger *log.Logger
	gb     *gobreaker.CircuitBreaker
}

func shouldBeSwitchedToOpen(counts gobreaker.Counts) bool {
	failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
	return counts.Requests >= 3 && failureRatio >= 0.6
}

func NewClientCircuitBreakerProxy() *ClientCircuitBreakerProxy {
	logger := log.New(os.Stdout, "CB\t", log.LstdFlags)

	cfg := gobreaker.Settings{
		Interval:    5 * time.Second,
		Timeout:     7 * time.Second,
		ReadyToTrip: shouldBeSwitchedToOpen,
		OnStateChange: func(_ string, from gobreaker.State, to gobreaker.State) {
			logger.Println("state changed from", from.String(), "to", to.String())
		},
	}

	return &ClientCircuitBreakerProxy{
		logger: logger,
		gb:     gobreaker.NewCircuitBreaker(cfg),
	}
}

func (c *ClientCircuitBreakerProxy) Send(endpoint string) (interface{}, error) {
	data, err := c.gb.Execute(func() (interface{}, error) {
		resp, err := http.Get(endpoint)
		if err != nil {
			log.Fatalln(err)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Fatalln(err)
		}
		return string(body), err
	})
	return data, err
}

type cacheHelper struct {
	resdis *redis.Client
}

type CacheHelper interface {
	GetInterface(ctx context.Context, key string, value interface{}) (interface{}, error)
	Set(ctx context.Context, key string, value interface{}, exppiration time.Duration) error
}

func NewCacheHelper(dns string) CacheHelper {
	rdb := redis.NewClient(&redis.Options{
		Addr:     dns,
		Password: "",
		DB:       0,
	})
	return &cacheHelper{
		resdis: rdb,
	}
}

func (h *cacheHelper) GetInterface(ctx context.Context, key string, value interface{}) (interface{}, error) {
	data, err := h.resdis.Get(key).Result()
	if err != nil {
		return nil, err
	}

	typeValue := reflect.TypeOf(data)
	kind := typeValue.Kind()

	var outData interface{}
	switch kind {
	case reflect.Ptr, reflect.Struct, reflect.Slice:
		outData = reflect.New(typeValue).Interface()
	default:
		outData = reflect.Zero(typeValue).Interface()
	}
	err = json.Unmarshal([]byte(data), &outData)
	if err != nil {
		return nil, err
	}
	switch kind {
	case reflect.Ptr, reflect.Struct, reflect.Slice:
		return reflect.ValueOf(outData).Interface(), nil
	}
	var outValue interface{} = outData
	if reflect.TypeOf(outData).ConvertibleTo(typeValue) {
		outValueConverted := reflect.ValueOf(outData).Convert(typeValue)
		outValue = outValueConverted.Interface()
	}
	if outValue == nil {
		return nil, errors.New("")
	}
	return outData, nil
}

func (h *cacheHelper) Set(ctx context.Context, key string, value interface{}, exppiration time.Duration) error {
	data, err := json.Marshal(&value)
	if err != nil {
		return err
	}
	_ = h.resdis.Set(key, data, exppiration)
	return nil
}
