# WhatsApp Message Decryption Issue Investigation

## Issue Summary

Messages can't be decrypted and when trying to request them from the phone, the system fails with error: "can't send message to unknown server lid"

## Debug Log Analysis

From the debug logs we can see:

```
DEBUG GREETINGS: Starting delayed phone request for message 7827EA62FC37473D5D907A02AF0450CE from 5493815153323@s.whatsapp.net
DEBUG GREETINGS: Session check for 5493815153323:0: hasSession=false, error=<nil>
DEBUG GREETINGS: Own ID: 5493517373811:64@s.whatsapp.net, Own LID: 173796388548816:64@lid
DEBUG GREETINGS: Found LID 50457410056305@lid for PN 5493815153323@s.whatsapp.net
DEBUG GREETINGS: Session check for LID 50457410056305_1:0: hasSession=false, error=<nil>
DEBUG GREETINGS: About to send phone request for 7827EA62FC37473D5D907A02AF0450CE to 5493517373811@s.whatsapp.net
DEBUG GREETINGS: Session check for own PN 5493517373811:0: hasSession=false, error=<nil>
DEBUG GREETINGS: Session check for own LID 173796388548816_1:64: hasSession=true, error=<nil>
DEBUG GREETINGS: Using LID 173796388548816@lid for phone request instead of PN
DEBUG GREETINGS: Failed to send request for unavailable message 7827EA62FC37473D5D907A02AF0450CE to phone: can't send message to unknown server lid
```

## Root Cause Analysis

The issue is in the `SendMessage` function in `send.go`. The code determines that it should use LID (`173796388548816@lid`) for the phone request because:

1. No session exists with own PN (`5493517373811:0`)
2. A session exists with own LID (`173796388548816_1:64`)
3. So it sets `phoneRequestTarget = ownLIDForPhone.ToNonAD()` which results in a JID with server type `types.HiddenUserServer` ("lid")

However, in the main `switch` statement (lines 361-374 in `send.go`), only these server types are handled:

-   `types.GroupServer`, `types.BroadcastServer`
-   `types.DefaultUserServer`, `types.BotServer`
-   `types.NewsletterServer`

The `types.HiddenUserServer` (which LIDs use) is **not included** in the switch statement, causing it to fall through to the `default` case which returns `ErrUnknownServer`.

## Code Analysis

In `send.go` around line 307, there is special preparation logic for `types.HiddenUserServer`:

```go
} else if to.Server == types.HiddenUserServer {
    ownID = cli.getOwnLID()
    extraParams.addressingMode = types.AddressingModeLID
}
```

But this handling is missing from the main switch statement where the actual sending happens.

## Solution

The fix is to add `types.HiddenUserServer` to the appropriate case in the switch statement. Since LID messages are direct messages (like `types.DefaultUserServer`), they should be handled by the same logic as `types.DefaultUserServer`.

## Fix Implementation

✅ **COMPLETED**: Modified the switch statement in `send.go` line 365 to include `types.HiddenUserServer` alongside `types.DefaultUserServer` and `types.BotServer`.

**Before:**

```go
case types.DefaultUserServer, types.BotServer:
```

**After:**

```go
case types.DefaultUserServer, types.BotServer, types.HiddenUserServer:
```

This ensures that when the phone request system tries to send a message to a LID (which has server type `types.HiddenUserServer`), it will be handled by the same direct message logic used for regular phone numbers.

## Files Involved

-   `send.go` - Main fix location (switch statement around line 365) ✅ **FIXED**
-   `retry.go` - Contains the phone request logic that triggers the issue
-   `types/jid.go` - Defines the server type constants

## Status

-   [x] Issue identified
-   [x] Root cause found
-   [x] Fix implemented
-   [x] Testing completed

## Expected Result

After this fix, when the phone request logic determines that it should use a LID instead of a phone number for sending the unavailable message request, the `SendMessage` function will now properly handle the `types.HiddenUserServer` and route it through the direct message (`sendDM`) logic, which should resolve the "can't send message to unknown server lid" error.

## Testing

-   ✅ Code compiles successfully (`go build .`)
-   ✅ All tests pass (`go test ./... -short`)
-   ✅ No existing functionality is broken

## Conclusion

The fix should resolve the decryption issue by allowing phone requests to be sent using LID addresses when no PN session is available but an LID session exists. The modification is minimal and follows the existing pattern in the codebase where LID messages are treated as direct messages similar to regular phone number messages.
