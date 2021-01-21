package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"

	"github.com/stripe/stripe-go/v72"
	"github.com/stripe/stripe-go/v72/checkout/session"
	"github.com/stripe/stripe-go/v72/paymentintent"
	"github.com/stripe/stripe-go/v72/webhook"
	"gopkg.in/yaml.v2"
)

type configStructure struct {
	Key struct {
		Publishable string `yaml:"publishable"`
		Secret      string `yaml:"secret"`
		Webhook     string `yaml:"webhook"`
	} `yaml:"key"`
	Host struct {
		Domain string `yaml:"domain"`
		Port   string `yaml:"port"`
	} `yaml:"host"`
	Discord struct {
		Webhook string `yaml:"webhook"`
		Contact string `yaml:"contact"`
	} `yaml:"discord"`
}

func loadConfig() configStructure {
	data, err := ioutil.ReadFile("config.yml")
	if err != nil {
		log.Panicf("Failed reading config: %v", err)
	}

	config := configStructure{}
	yaml.UnmarshalStrict(data, &config)

	return config
}

var config = loadConfig()

func loadTemplate(name string) *template.Template {
	return template.Must(template.ParseFiles("static/" + name + ".html"))
}

var templateCheckout = loadTemplate("checkout")
var templateCancel = loadTemplate("cancel")
var templateSuccess = loadTemplate("success")

type genericError struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, statusCode int, message string) {
	w.WriteHeader(statusCode)
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(genericError{Error: message})
	_, err := w.Write(data)
	if err != nil {
		log.Printf("Failed writing response: %v\n", err)
	}
}

type sessionCreateRequest struct {
	Price   string
	Name    string
	Fortune string
}

type sessionCreateResponse struct {
	SessionID string
}

func endpointSessionCreate(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, 65535)
	requestBody, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Printf("Failed reading request: %v\n", err)
		return
	}

	requestData := sessionCreateRequest{}
	err = json.Unmarshal(requestBody, &requestData)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Malformed request")
		return
	}

	price, err := strconv.Atoi(requestData.Price)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Malformed request")
		return
	}

	if price < 100 {
		writeError(w, http.StatusBadRequest, "Price too low")
		return
	}

	if len(requestData.Name) > 300 {
		writeError(w, http.StatusBadRequest, "Name too long")
		return
	}

	if len(requestData.Fortune) > 3000 {
		writeError(w, http.StatusBadRequest, "Fortune too long")
		return
	}

	params := &stripe.CheckoutSessionParams{
		PaymentMethodTypes: stripe.StringSlice([]string{
			"card",
		}),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency: stripe.String(string(stripe.CurrencyUSD)),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String("Fortune Telling"),
					},
					UnitAmount: stripe.Int64(int64(price)),
				},
				Quantity: stripe.Int64(1),
			},
		},
		Mode:       stripe.String(string(stripe.CheckoutSessionModePayment)),
		SuccessURL: stripe.String(config.Host.Domain + "/success?session_id={CHECKOUT_SESSION_ID}"),
		CancelURL:  stripe.String(config.Host.Domain + "/cancel"),
	}

	params.AddMetadata("name", requestData.Name)
	params.AddMetadata("fortune", requestData.Fortune)

	session, err := session.New(params)
	if err != nil {
		log.Printf("Failed creating session: %v\n", err)
		return
	}

	log.Printf("Created new checkout session for %v\n", req.RemoteAddr)

	data, _ := json.Marshal(sessionCreateResponse{
		SessionID: session.ID,
	})
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

type discordField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type discordEmbed struct {
	Title  string         `json:"title"`
	Color  int            `json:"color"`
	Fields []discordField `json:"fields"`
}

type discordWebhook struct {
	Embeds []discordEmbed `json:"embeds"`
}

func endpointWebhookCallback(w http.ResponseWriter, req *http.Request) {
	req.Body = http.MaxBytesReader(w, req.Body, 65536)
	payload, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Printf("Failed reading request: %v\n", err)
		return
	}

	event, err := webhook.ConstructEvent(payload, req.Header.Get("Stripe-Signature"), config.Key.Webhook)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Malformed request")
		return
	}

	if event.Type != "checkout.session.completed" {
		return // bruh this ain't what it's supposed to be
	}
	var checkoutSession stripe.CheckoutSession

	err = json.Unmarshal(event.Data.Raw, &checkoutSession)
	if err != nil {
		log.Printf("Failed parsing event: %v\n", err)
		return
	}

	username, exists := checkoutSession.Metadata["name"]
	if !exists {
		return // not interested
	}

	fortune, exists := checkoutSession.Metadata["fortune"]
	if !exists {
		return // not interested
	}

	paymentIntent, err := paymentintent.Get(checkoutSession.PaymentIntent.ID, nil)
	includePrice := err == nil

	webhookStructure := discordWebhook{
		Embeds: make([]discordEmbed, 0),
	}
	webhookStructure.Embeds = append(webhookStructure.Embeds, discordEmbed{
		Title:  "New Request",
		Color:  0x00cc00,
		Fields: make([]discordField, 0),
	})
	webhookStructure.Embeds[0].Fields = append(webhookStructure.Embeds[0].Fields, discordField{
		Name:  "Name",
		Value: username,
	}, discordField{
		Name:  "Fortune",
		Value: fortune,
	})
	if includePrice {
		webhookStructure.Embeds[0].Fields = append(webhookStructure.Embeds[0].Fields, discordField{
			Name:  "Price",
			Value: "$" + fmt.Sprintf("%.2f", float64(paymentIntent.Amount)/100.0),
		})
	}

	buffer := bytes.NewBuffer(make([]byte, 0))
	json.NewEncoder(buffer).Encode(&webhookStructure)
	resp, err := http.Post(config.Discord.Webhook, "application/json", buffer)
	if err != nil {
		log.Printf("Failed executing webhook: %v\n", err)
		return
	}
	resp.Body.Close()

	w.WriteHeader(http.StatusOK)
}

type staticTemplateExecuter struct {
	data []byte
}

func newStaticTemplateExecuter(tmplt *template.Template, data interface{}) *staticTemplateExecuter {
	buffer := bytes.NewBuffer(make([]byte, 0))
	err := tmplt.Execute(buffer, data)
	if err != nil {
		log.Panicf("Failed executing template %s: %v", tmplt.Name(), err)
	}
	return &staticTemplateExecuter{
		data: buffer.Bytes(),
	}
}

func (e *staticTemplateExecuter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(200)
	w.Header().Set("Content-Type", "text/html")
	_, err := w.Write(e.data)
	if err != nil {
		log.Printf("Failed writing response: %v\n", err)
	}
}

func main() {
	stripe.Key = config.Key.Secret
	http.HandleFunc("/session", endpointSessionCreate)
	http.HandleFunc("/webhook", endpointWebhookCallback)
	http.Handle("/", newStaticTemplateExecuter(templateCheckout, map[string]string{
		"StripeKey": config.Key.Publishable,
		"Contact":   config.Discord.Contact,
	}))
	http.Handle("/cancel", newStaticTemplateExecuter(templateCancel, map[string]string{
		"CheckoutPage": config.Host.Domain,
	}))
	http.Handle("/success", newStaticTemplateExecuter(templateSuccess, map[string]string{
		"CheckoutPage": config.Host.Domain,
	}))

	log.Printf("Listening on %s\n", config.Host.Domain)
	log.Fatal(http.ListenAndServe("localhost:"+config.Host.Port, nil))
}
