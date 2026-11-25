# Run by the job heartbeat with sh -ec. valkey-cli reads the password from
# VALKEYCLI_AUTH (REDISCLI_AUTH in older versions).
host=$(echo "$CACHE_ADDR" | cut -d: -f1)
port=$(echo "$CACHE_ADDR" | cut -d: -f2)

if wget -q -O /dev/null "http://$TARGET_ADDR$HEALTH_PATH"; then
  valkey-cli -h "$host" -p "$port" INCR "heartbeat:$TARGET_NAME:ok"
else
  valkey-cli -h "$host" -p "$port" INCR "heartbeat:$TARGET_NAME:failed"
  exit 1
fi
