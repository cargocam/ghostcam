use serde::{Deserialize, Serialize};

use crate::ingest::demand::ClientMode;

/// Messages received from the client on the commands data channel (JSON).
#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ClientMessage {
    ClientMode { mode: ClientModeWire },
}

/// Wire representation of client mode.
#[derive(Debug, Clone, Copy, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ClientModeWire {
    Live,
    Playback,
    Map,
}

impl From<ClientModeWire> for ClientMode {
    fn from(w: ClientModeWire) -> Self {
        match w {
            ClientModeWire::Live => ClientMode::Live,
            ClientModeWire::Playback => ClientMode::Playback,
            ClientModeWire::Map => ClientMode::Map,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_client_mode_live() {
        let msg: ClientMessage =
            serde_json::from_str(r#"{"type":"client_mode","mode":"live"}"#).unwrap();
        assert!(matches!(
            msg,
            ClientMessage::ClientMode {
                mode: ClientModeWire::Live
            }
        ));
    }

    #[test]
    fn parse_client_mode_playback() {
        let msg: ClientMessage =
            serde_json::from_str(r#"{"type":"client_mode","mode":"playback"}"#).unwrap();
        assert!(matches!(
            msg,
            ClientMessage::ClientMode {
                mode: ClientModeWire::Playback
            }
        ));
    }

    #[test]
    fn parse_client_mode_map() {
        let msg: ClientMessage =
            serde_json::from_str(r#"{"type":"client_mode","mode":"map"}"#).unwrap();
        assert!(matches!(
            msg,
            ClientMessage::ClientMode {
                mode: ClientModeWire::Map
            }
        ));
    }

    #[test]
    fn parse_unknown_message() {
        let result =
            serde_json::from_str::<ClientMessage>(r#"{"type":"unknown","foo":"bar"}"#);
        assert!(result.is_err());
    }
}
