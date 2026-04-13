#!/usr/bin/env bash
#
# Seed a test camera with thousands of synthetic segments in Postgres + MinIO
# so you can stress-test the "Delete All Footage" flow locally.
#
# Usage:
#   ./scripts/seed-segments.sh [COUNT]    # default 4000
#
# Prerequisites:
#   - docker compose stack running (docker compose up -d --profile test)
#   - At least one camera enrolled (let test cameras run for ~30s first)
#
# What it does:
#   1. Picks the first camera from the DB
#   2. Creates COUNT tiny (~2 KB) S3 objects in MinIO
#   3. Inserts COUNT segment rows into Postgres pointing at those objects
#   4. Updates the Redis storage counter to match
#
# Then open localhost:5173, go to camera settings → Delete All Footage
# and verify the progress bar + no 502.

set -euo pipefail

COUNT="${1:-4000}"
SEGMENT_DURATION_MS=6000  # 6 seconds per segment, matching real cameras

echo "==> Picking first enrolled camera..."
DEVICE_ID=$(docker compose exec -T postgres psql -U ghostcam -d ghostcam -tAc \
  "SELECT device_id FROM cameras LIMIT 1")

if [ -z "$DEVICE_ID" ]; then
  echo "ERROR: No cameras found. Start test cameras first:"
  echo "  docker compose up -d --profile test"
  echo "  # wait ~30s for enrollment, then re-run this script"
  exit 1
fi

USER_ID=$(docker compose exec -T postgres psql -U ghostcam -d ghostcam -tAc \
  "SELECT user_id FROM cameras WHERE device_id = '$DEVICE_ID'")

echo "    device_id: $DEVICE_ID"
echo "    user_id:   $USER_ID"
echo "    count:     $COUNT"

# Base timestamp: 24 hours ago, so segments don't collide with real ones
BASE_TS=$(( ($(date +%s) - 86400) * 1000 ))

echo "==> Creating $COUNT S3 objects in MinIO..."
# Create a tiny file to use as the segment body
TMPFILE=$(mktemp)
dd if=/dev/urandom of="$TMPFILE" bs=2048 count=1 2>/dev/null

# Upload objects in parallel batches via mc inside the minio container
# First, copy the tiny file into the minio container
docker compose cp "$TMPFILE" minio:/tmp/seed-segment.ts
rm "$TMPFILE"

# Generate mc cp commands in batches of 200
BATCH_SIZE=200
for (( i=0; i<COUNT; i+=BATCH_SIZE )); do
  END=$((i + BATCH_SIZE))
  if [ $END -gt $COUNT ]; then END=$COUNT; fi

  # Build a script that copies the file to each key
  CMD=""
  for (( j=i; j<END; j++ )); do
    S3_KEY="segments/${DEVICE_ID}/$((BASE_TS + j * SEGMENT_DURATION_MS)).ts"
    CMD="${CMD}mc cp /tmp/seed-segment.ts local/ghostcam-segments/${S3_KEY} 2>/dev/null;"
  done

  docker compose exec -T minio sh -c "$CMD" &

  # Limit background jobs to avoid overwhelming the container
  if (( (i / BATCH_SIZE + 1) % 10 == 0 )); then
    wait
    echo "    S3: $END / $COUNT objects"
  fi
done
wait
echo "    S3: $COUNT / $COUNT objects done"

echo "==> Inserting $COUNT segment rows into Postgres..."
# Build a single SQL file with a multi-row INSERT for speed
SQL="INSERT INTO segments (segment_id, device_id, s3_key, start_ts, end_ts, size_bytes, resolution, created_at, has_motion) VALUES"
for (( i=0; i<COUNT; i++ )); do
  START_TS=$((BASE_TS + i * SEGMENT_DURATION_MS))
  END_TS=$((START_TS + SEGMENT_DURATION_MS))
  SEG_ID=$(uuidgen 2>/dev/null || cat /proc/sys/kernel/random/uuid 2>/dev/null || python3 -c "import uuid; print(uuid.uuid4())")
  S3_KEY="segments/${DEVICE_ID}/${START_TS}.ts"
  COMMA=","
  if [ $i -eq $((COUNT - 1)) ]; then COMMA=";"; fi
  SQL="${SQL}
('${SEG_ID}', '${DEVICE_ID}', '${S3_KEY}', ${START_TS}, ${END_TS}, 2048, '720p', ${START_TS}, false)${COMMA}"

  if (( (i + 1) % 1000 == 0 )); then
    echo "    SQL: built $((i + 1)) / $COUNT rows"
  fi
done

echo "$SQL" | docker compose exec -T postgres psql -U ghostcam -d ghostcam -q

echo "==> Updating Redis storage counter..."
TOTAL_BYTES=$((COUNT * 2048))
docker compose exec -T redis redis-cli INCRBY "storage_bytes:${USER_ID}" "$TOTAL_BYTES" > /dev/null

echo ""
echo "Done! Seeded $COUNT segments (~$((TOTAL_BYTES / 1024)) KB) for camera $DEVICE_ID"
echo ""
echo "Now open http://localhost:5173, go to camera settings → Delete All Footage"
echo "and verify:"
echo "  1. Progress bar fills smoothly"
echo "  2. No 502 timeout"
echo "  3. Deletion completes successfully"
