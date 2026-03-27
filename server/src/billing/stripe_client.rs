use std::collections::HashMap;

use anyhow::{Context, Result};
use stripe::{
    BillingPortalSession, CheckoutSession, CheckoutSessionMode, Client, CreateBillingPortalSession,
    CreateCheckoutSession, CreateCheckoutSessionLineItems, CreateCustomer, Customer, CustomerId,
    EventObject, EventType, Webhook,
};

pub struct StripeClient {
    client: Client,
    webhook_secret: String,
    /// tier_id -> stripe_price_id
    price_ids: HashMap<String, String>,
}

/// Parsed Stripe webhook event with the fields we care about.
pub enum WebhookAction {
    CheckoutCompleted {
        customer_id: String,
        subscription_id: String,
        client_reference_id: Option<String>,
    },
    SubscriptionUpdated {
        customer_id: String,
        subscription_id: String,
        status: String,
        current_period_start: Option<u64>,
        current_period_end: Option<u64>,
        price_id: Option<String>,
    },
    SubscriptionDeleted {
        customer_id: String,
    },
    InvoicePaymentSucceeded {
        customer_id: String,
    },
    InvoicePaymentFailed {
        customer_id: String,
    },
    Unknown,
}

impl StripeClient {
    pub fn new(secret_key: &str, webhook_secret: &str, price_ids: HashMap<String, String>) -> Self {
        Self {
            client: Client::new(secret_key),
            webhook_secret: webhook_secret.to_string(),
            price_ids,
        }
    }

    pub fn price_id_for_tier(&self, tier: &str) -> Option<&String> {
        self.price_ids.get(tier)
    }

    /// Reverse lookup: stripe price ID -> tier ID
    pub fn tier_for_price_id(&self, price_id: &str) -> Option<String> {
        self.price_ids
            .iter()
            .find(|(_, v)| v.as_str() == price_id)
            .map(|(k, _)| k.clone())
    }

    pub async fn create_customer(&self, email: &str, user_id: &str) -> Result<String> {
        let mut params = CreateCustomer::new();
        params.email = Some(email);
        params.metadata = Some(HashMap::from([("user_id".into(), user_id.into())]));
        let customer = Customer::create(&self.client, params)
            .await
            .context("failed to create Stripe customer")?;
        Ok(customer.id.to_string())
    }

    pub async fn create_checkout_session(
        &self,
        customer_id: &str,
        price_id: &str,
        user_id: &str,
        success_url: &str,
        cancel_url: &str,
    ) -> Result<String> {
        let cid: CustomerId = customer_id.parse().context("invalid customer ID")?;
        let mut params = CreateCheckoutSession::new();
        params.customer = Some(cid);
        params.mode = Some(CheckoutSessionMode::Subscription);
        params.success_url = Some(success_url);
        params.cancel_url = Some(cancel_url);
        params.client_reference_id = Some(user_id);
        params.line_items = Some(vec![CreateCheckoutSessionLineItems {
            price: Some(price_id.to_string()),
            quantity: Some(1),
            ..Default::default()
        }]);

        let session = CheckoutSession::create(&self.client, params)
            .await
            .context("failed to create Stripe checkout session")?;

        session
            .url
            .ok_or_else(|| anyhow::anyhow!("Stripe checkout session missing URL"))
    }

    pub async fn create_portal_session(
        &self,
        customer_id: &str,
        return_url: &str,
        portal_config_id: Option<&str>,
    ) -> Result<String> {
        let cid: CustomerId = customer_id.parse().context("invalid customer ID")?;
        let mut params = CreateBillingPortalSession::new(cid);
        params.return_url = Some(return_url);
        if let Some(config_id) = portal_config_id {
            params.configuration = Some(config_id);
        }

        let session = BillingPortalSession::create(&self.client, params)
            .await
            .context("failed to create Stripe portal session")?;

        Ok(session.url)
    }

    pub fn verify_webhook(&self, payload: &str, signature: &str) -> Result<stripe::Event> {
        Webhook::construct_event(payload, signature, &self.webhook_secret)
            .map_err(|e| anyhow::anyhow!("webhook signature verification failed: {e}"))
    }

    /// Parse a verified Stripe event into a simplified action.
    pub fn parse_event(&self, event: &stripe::Event) -> WebhookAction {
        match event.type_ {
            EventType::CheckoutSessionCompleted => {
                if let EventObject::CheckoutSession(ref session) = event.data.object {
                    WebhookAction::CheckoutCompleted {
                        customer_id: session
                            .customer
                            .as_ref()
                            .map(|c| c.id().to_string())
                            .unwrap_or_default(),
                        subscription_id: session
                            .subscription
                            .as_ref()
                            .map(|s| s.id().to_string())
                            .unwrap_or_default(),
                        client_reference_id: session.client_reference_id.clone(),
                    }
                } else {
                    WebhookAction::Unknown
                }
            }
            EventType::CustomerSubscriptionUpdated => {
                if let EventObject::Subscription(ref sub) = event.data.object {
                    let price_id = sub
                        .items
                        .data
                        .first()
                        .and_then(|item| item.price.as_ref())
                        .map(|p| p.id.to_string());
                    WebhookAction::SubscriptionUpdated {
                        customer_id: sub.customer.id().to_string(),
                        subscription_id: sub.id.to_string(),
                        status: sub.status.as_str().to_string(),
                        current_period_start: Some(sub.current_period_start as u64),
                        current_period_end: Some(sub.current_period_end as u64),
                        price_id,
                    }
                } else {
                    WebhookAction::Unknown
                }
            }
            EventType::CustomerSubscriptionDeleted => {
                if let EventObject::Subscription(ref sub) = event.data.object {
                    WebhookAction::SubscriptionDeleted {
                        customer_id: sub.customer.id().to_string(),
                    }
                } else {
                    WebhookAction::Unknown
                }
            }
            EventType::InvoicePaymentSucceeded => {
                if let EventObject::Invoice(ref invoice) = event.data.object {
                    WebhookAction::InvoicePaymentSucceeded {
                        customer_id: invoice
                            .customer
                            .as_ref()
                            .map(|c| c.id().to_string())
                            .unwrap_or_default(),
                    }
                } else {
                    WebhookAction::Unknown
                }
            }
            EventType::InvoicePaymentFailed => {
                if let EventObject::Invoice(ref invoice) = event.data.object {
                    WebhookAction::InvoicePaymentFailed {
                        customer_id: invoice
                            .customer
                            .as_ref()
                            .map(|c| c.id().to_string())
                            .unwrap_or_default(),
                    }
                } else {
                    WebhookAction::Unknown
                }
            }
            _ => WebhookAction::Unknown,
        }
    }
}
