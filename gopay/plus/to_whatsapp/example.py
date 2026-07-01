import os
import time
import wa_relay

def main():
    print("=== WhatsApp Relay Demo ===")

    # Check if already running
    st = wa_relay.status()
    if st.get("running"):
        print("Relay is already running. Stopping it first...")
        wa_relay.stop()

    pairing_phone = os.environ.get("WA_PAIRING_PHONE", "")

    print("Starting WhatsApp Relay in pairing mode...")
    st = wa_relay.start(pairing_phone=pairing_phone)

    printed = False
    try:
        while True:
            st = wa_relay.status()
            status_text = st.get("status")

            if status_text == "awaiting_pairing_code" and not printed:
                print("\n" + "="*40)
                print(f"PAIRING CODE: {st.get('code')}")
                print("Please enter this code in your WhatsApp app -> Linked Devices -> Link with phone number instead")
                print("="*40 + "\n")
                printed = True

            elif status_text == "connected":
                if printed:
                    print("\nSuccessfully connected!")
                    printed = False # Reset for future reconnections if needed

            elif status_text == "error":
                print(f"Error: {st.get('error')}")
                break

            elif status_text == "disconnected":
                print(f"Disconnected: {st.get('reason')}")
                # Wait for auto-reconnect

            time.sleep(1)

    except KeyboardInterrupt:
        print("\nStopping...")

    finally:
        wa_relay.stop()
        print("Relay stopped.")

if __name__ == "__main__":
    main()
