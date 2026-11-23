package main

// DareMoto: "apimain stripe-setup" prepares the Stripe account for DareMoto billing.
// Idempotent: run it once in test mode and again after switching to the live key.
//   - products and monthly EUR prices (VAT excluded), found again by lookup_key
//   - customer portal configuration (payment methods, invoices, cancellation)
//   - webhook endpoint <public url>/api/billing/webhook
// The webhook signing secret is written to data/billing.json (mode 0600) and never printed.
// Restart the api container afterwards so it reads the new file.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/lejianwen/rustdesk-api/v2/service"
	"github.com/spf13/cobra"
	"github.com/stripe/stripe-go/v85"
)

var (
	setupTeam, setupBusiness, setupExtra int64
	setupRecreateWebhook                 bool
	setupRecreatePortal                  bool
	setupTaxCode                         string
)

var stripeSetupCmd = &cobra.Command{
	Use:   "stripe-setup",
	Short: "Create DareMoto products, prices, portal configuration and webhook on Stripe",
	Run: func(cmd *cobra.Command, args []string) {
		if err := stripeSetup(context.Background()); err != nil {
			fmt.Println("ERRORE:", err)
			os.Exit(1)
		}
	},
}

func init() {
	stripeSetupCmd.Flags().Int64Var(&setupTeam, "team", 900, "Team price in cents per month, VAT excluded")
	stripeSetupCmd.Flags().Int64Var(&setupBusiness, "business", 2900, "Business price in cents per month, VAT excluded")
	stripeSetupCmd.Flags().Int64Var(&setupExtra, "extra-session", 800, "extra session price in cents per month, VAT excluded")
	// txcd_10103001 = Software as a Service (SaaS), business use
	stripeSetupCmd.Flags().StringVar(&setupTaxCode, "tax-code", "txcd_10103001", "Stripe product tax code")
	stripeSetupCmd.Flags().BoolVar(&setupRecreateWebhook, "recreate-webhook", false, "create a new webhook endpoint even if one is recorded")
	stripeSetupCmd.Flags().BoolVar(&setupRecreatePortal, "recreate-portal", false, "create a new portal configuration even if one is recorded")
}

func stripeSetup(ctx context.Context) error {
	bs := service.Billing
	if !bs.Enabled() {
		return fmt.Errorf("DAREMOTO_STRIPE_SECRET_KEY non impostata")
	}
	sc := bs.Client()
	key := os.Getenv("DAREMOTO_STRIPE_SECRET_KEY")
	live := strings.HasPrefix(key, "sk_live_") || strings.HasPrefix(key, "rk_live_")
	fmt.Printf("Modalità: %s\n", map[bool]string{true: "LIVE", false: "test"}[live])

	stored := service.BillingStored{}
	if b, err := os.ReadFile("data/billing.json"); err == nil {
		_ = json.Unmarshal(b, &stored)
	}
	if stored.Livemode != live {
		// test and live are separate worlds on Stripe: nothing recorded can be reused
		stored = service.BillingStored{}
	}
	stored.Livemode = live

	// 1. products and prices
	want := []struct {
		lookup, product, desc string
		amount                int64
	}{
		{service.LookupKeyTeam, "DareMoto Team", "Fino a 30 PC, 2 sessioni contemporanee", setupTeam},
		{service.LookupKeyBusiness, "DareMoto Business", "Fino a 100 PC, 5 sessioni contemporanee", setupBusiness},
		{service.LookupKeyExtraSession, "DareMoto Sessione extra", "Una sessione contemporanea in più", setupExtra},
	}
	existing := map[string]*stripe.Price{}
	lookups := []string{}
	for _, w := range want {
		lookups = append(lookups, w.lookup)
	}
	for p, err := range sc.V1Prices.List(ctx, &stripe.PriceListParams{Active: stripe.Bool(true), LookupKeys: stripe.StringSlice(lookups)}).All(ctx) {
		if err != nil {
			return err
		}
		existing[p.LookupKey] = p
	}
	for _, w := range want {
		if p := existing[w.lookup]; p != nil {
			fmt.Printf("Prezzo %-32s già presente: %s (%d cent/%s)\n", w.lookup, p.ID, p.UnitAmount, p.Recurring.Interval)
			// products created before the tax code was set: Managed Payments and Stripe Tax need it
			if p.Product != nil {
				prod, err := sc.V1Products.Retrieve(ctx, p.Product.ID, nil)
				if err != nil {
					return err
				}
				if prod.TaxCode == nil || prod.TaxCode.ID != setupTaxCode {
					if _, err := sc.V1Products.Update(ctx, prod.ID, &stripe.ProductUpdateParams{TaxCode: stripe.String(setupTaxCode)}); err != nil {
						return err
					}
					fmt.Printf("  prodotto %s: codice fiscale impostato a %s\n", prod.ID, setupTaxCode)
				}
			}
			continue
		}
		prod, err := sc.V1Products.Create(ctx, &stripe.ProductCreateParams{
			Name:        stripe.String(w.product),
			Description: stripe.String(w.desc),
			TaxCode:     stripe.String(setupTaxCode),
			Metadata:    map[string]string{"daremoto": w.lookup},
		})
		if err != nil {
			return err
		}
		price, err := sc.V1Prices.Create(ctx, &stripe.PriceCreateParams{
			Product:     stripe.String(prod.ID),
			Currency:    stripe.String("eur"),
			UnitAmount:  stripe.Int64(w.amount),
			Recurring:   &stripe.PriceCreateRecurringParams{Interval: stripe.String("month")},
			LookupKey:   stripe.String(w.lookup),
			TaxBehavior: stripe.String("exclusive"),
			Nickname:    stripe.String(w.product + " mensile"),
		})
		if err != nil {
			return err
		}
		fmt.Printf("Prezzo %-32s creato: %s (%d cent/mese)\n", w.lookup, price.ID, w.amount)
	}

	// 2. customer portal: plan changes happen on /account (multi-item subscriptions)
	if stored.PortalConfiguration == "" || setupRecreatePortal {
		pc, err := sc.V1BillingPortalConfigurations.Create(ctx, &stripe.BillingPortalConfigurationCreateParams{
			Name: stripe.String("DareMoto"),
			BusinessProfile: &stripe.BillingPortalConfigurationCreateBusinessProfileParams{
				Headline:          stripe.String("DareMoto by Ianus Project"),
				PrivacyPolicyURL:  stripe.String("https://daremoto.net/privacy/"),
				TermsOfServiceURL: stripe.String("https://daremoto.net/licenza/"),
			},
			DefaultReturnURL: stripe.String(bs.PublicURL() + "/account/"),
			Features: &stripe.BillingPortalConfigurationCreateFeaturesParams{
				CustomerUpdate: &stripe.BillingPortalConfigurationCreateFeaturesCustomerUpdateParams{
					Enabled:        stripe.Bool(true),
					AllowedUpdates: stripe.StringSlice([]string{"email", "address", "tax_id", "name"}),
				},
				InvoiceHistory:      &stripe.BillingPortalConfigurationCreateFeaturesInvoiceHistoryParams{Enabled: stripe.Bool(true)},
				PaymentMethodUpdate: &stripe.BillingPortalConfigurationCreateFeaturesPaymentMethodUpdateParams{Enabled: stripe.Bool(true)},
				SubscriptionCancel: &stripe.BillingPortalConfigurationCreateFeaturesSubscriptionCancelParams{
					Enabled: stripe.Bool(true),
					Mode:    stripe.String("at_period_end"),
				},
			},
		})
		if err != nil {
			return err
		}
		stored.PortalConfiguration = pc.ID
		fmt.Println("Configurazione portale clienti creata:", pc.ID)
	} else {
		fmt.Println("Configurazione portale clienti già presente:", stored.PortalConfiguration)
	}

	// 3. webhook endpoint
	if stored.WebhookEndpointId == "" || setupRecreateWebhook {
		url := bs.PublicURL() + "/api/billing/webhook"
		we, err := sc.V1WebhookEndpoints.Create(ctx, &stripe.WebhookEndpointCreateParams{
			URL:         stripe.String(url),
			Description: stripe.String("DareMoto account server"),
			APIVersion:  stripe.String(stripe.APIVersion),
			EnabledEvents: stripe.StringSlice([]string{
				"checkout.session.completed",
				"checkout.session.async_payment_succeeded",
				"customer.subscription.created",
				"customer.subscription.updated",
				"customer.subscription.deleted",
				"customer.subscription.paused",
				"customer.subscription.resumed",
				"invoice.paid",
				"invoice.payment_failed",
			}),
		})
		if err != nil {
			return err
		}
		stored.WebhookEndpointId = we.ID
		stored.WebhookSecret = we.Secret
		fmt.Printf("Webhook creato: %s -> %s (segreto salvato in data/billing.json)\n", we.ID, url)
	} else {
		fmt.Println("Webhook già presente:", stored.WebhookEndpointId)
	}

	b, _ := json.MarshalIndent(stored, "", "  ")
	if err := os.WriteFile("data/billing.json", b, 0600); err != nil {
		return err
	}
	fmt.Println("Fatto. Riavvia il container api per caricare la configurazione.")
	return nil
}
