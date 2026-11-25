# Run by the hook job smoke (smoke.norca.hcl) with sh -ec: asks the service
# for its health path until it answers, at most ten times.
for i in 1 2 3 4 5 6 7 8 9 10; do
  if curl -fsS --max-time 5 "$TARGET" >/dev/null; then
    echo "healthy: $TARGET"
    exit 0
  fi
  echo "not healthy yet ($i): $TARGET"
  sleep 3
done
echo "no healthy answer from $TARGET" >&2
exit 1
