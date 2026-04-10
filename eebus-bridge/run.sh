#!/usr/bin/with-contenv bashio

# Auto-discover MQTT credentials from the Mosquitto addon via the Services API.
# Manual options (if set) take precedence.
if bashio::config.has_value 'MQTT_HOST'; then
  export MQTT_HOST="$(bashio::config 'MQTT_HOST')"
else
  export MQTT_HOST="$(bashio::services mqtt 'host')"
fi

if bashio::config.has_value 'MQTT_PORT'; then
  export MQTT_PORT="$(bashio::config 'MQTT_PORT')"
else
  export MQTT_PORT="$(bashio::services mqtt 'port')"
fi

if bashio::config.has_value 'MQTT_USER'; then
  export MQTT_USER="$(bashio::config 'MQTT_USER')"
else
  export MQTT_USER="$(bashio::services mqtt 'username')"
fi

if bashio::config.has_value 'MQTT_PWD'; then
  export MQTT_PWD="$(bashio::config 'MQTT_PWD')"
else
  export MQTT_PWD="$(bashio::services mqtt 'password')"
fi

export EEBUS_PORT="$(bashio::config 'eebus_port')"
export MQTT_DISCOVERY_PREFIX="$(bashio::config 'discovery_prefix')"
export MQTT_TOPIC_PREFIX="$(bashio::config 'topic_prefix')"

exec /app/eebus-ha-bridge
