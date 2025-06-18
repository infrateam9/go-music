import sys
from urllib.parse import urlparse, parse_qs
from datetime import datetime, timedelta

if len(sys.argv) != 2:
    print("Usage: python check_presigned_url_expiry.py <presigned_url>")
    sys.exit(1)

url = sys.argv[1]
parsed = urlparse(url)
params = parse_qs(parsed.query)

amz_date = params.get('X-Amz-Date', [None])[0]
expires = params.get('X-Amz-Expires', [None])[0]

if not amz_date or not expires:
    print("Could not find X-Amz-Date or X-Amz-Expires in the URL.")
    sys.exit(1)

# Parse date and expiry
try:
    dt = datetime.strptime(amz_date, "%Y%m%dT%H%M%SZ")
    expires = int(expires)
except Exception as e:
    print(f"Error parsing date or expires: {e}")
    sys.exit(1)

expiry_time = dt + timedelta(seconds=expires)
now = datetime.utcnow()

print(f"URL generated at: {dt} UTC")
print(f"Expires at:      {expiry_time} UTC")
print(f"Current time:    {now} UTC")
if now < expiry_time:
    print("The pre-signed URL is still valid.")
else:
    print("The pre-signed URL has expired.")
