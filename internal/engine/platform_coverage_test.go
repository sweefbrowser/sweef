package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// platformImplemented lists every API name the web-platform layer provides
// (docs/web-platform.md). Dotted paths are walked from globalThis.
var platformImplemented = []string{
	// streams and codecs
	"ReadableStream", "ReadableStreamDefaultReader", "ReadableStreamBYOBReader", "ReadableStreamDefaultController", "ReadableByteStreamController", "ReadableStreamBYOBRequest",
	"WritableStream", "WritableStreamDefaultWriter", "WritableStreamDefaultController", "TransformStream", "TransformStreamDefaultController",
	"CountQueuingStrategy", "ByteLengthQueuingStrategy", "CompressionStream", "DecompressionStream", "TextEncoderStream", "TextDecoderStream",
	// XPath, URLPattern
	"XPathEvaluator", "XPathExpression", "XPathResult", "XPathNSResolver", "document.evaluate", "document.createExpression", "document.createNSResolver", "URLPattern",
	// ECMAScript and scheduling
	"Array.fromAsync", "Symbol.dispose", "Symbol.asyncDispose", "DisposableStack", "AsyncDisposableStack", "SuppressedError", "Atomics.waitAsync", "Intl.Locale",
	"Iterator", "Iterator.from", "Iterator.prototype.map", "Set.prototype.union", "Set.prototype.isSubsetOf", "Object.groupBy", "Map.groupBy", "Promise.withResolvers", "Promise.try",
	"Array.prototype.toSorted", "ArrayBuffer.prototype.transfer", "String.prototype.isWellFormed", "RegExp.escape", "Error.captureStackTrace",
	"scheduler", "scheduler.postTask", "scheduler.yield", "Scheduler", "TaskController", "TaskSignal", "TaskPriorityChangeEvent",
	// navigation, transitions, internals, CSS
	"ViewTransition", "document.startViewTransition", "document.activeViewTransition",
	"navigation", "Navigation", "NavigationHistoryEntry", "NavigationDestination", "NavigationTransition", "NavigationActivation", "NavigateEvent", "NavigationCurrentEntryChangeEvent",
	"ElementInternals", "CustomStateSet", "ValidityState", "Element.prototype.attachInternals",
	"Highlight", "HighlightRegistry", "CSS.highlights", "CSS.registerProperty", "CSSPropertyRule", "CSSRule", "AbstractRange", "ContentVisibilityAutoStateChangeEvent",
	// devices and platform objects
	"ScreenOrientation", "screen.orientation", "DeviceOrientationEvent", "DeviceMotionEvent", "DeviceMotionEventAcceleration", "DeviceMotionEventRotationRate",
	"Gamepad", "GamepadButton", "GamepadEvent", "navigator.getGamepads", "WakeLock", "WakeLockSentinel", "navigator.wakeLock",
	"MediaMetadata", "MediaSession", "navigator.mediaSession", "CredentialsContainer", "Credential", "PasswordCredential", "FederatedCredential", "PublicKeyCredential", "navigator.credentials",
	"PaymentRequest", "PaymentResponse", "PaymentRequestUpdateEvent", "document.hasStorageAccess", "document.requestStorageAccess",
	"trustedTypes", "TrustedHTML", "TrustedScript", "TrustedScriptURL", "TrustedTypePolicy", "TrustedTypePolicyFactory",
	"PictureInPictureWindow", "PictureInPictureEvent", "document.pictureInPictureEnabled", "document.exitPictureInPicture", "HTMLVideoElement.prototype.requestPictureInPicture",
	// file system
	"showOpenFilePicker", "showSaveFilePicker", "showDirectoryPicker", "FileSystemHandle", "FileSystemFileHandle", "FileSystemDirectoryHandle", "FileSystemWritableFileStream",
	"StorageManager", "navigator.storage", "navigator.storage.getDirectory",
	// notifications, push, sync, permissions (pwa.js)
	"Notification", "NotificationEvent", "Permissions", "PermissionStatus", "navigator.permissions", "PushManager", "PushSubscription", "PushSubscriptionOptions", "PushEvent", "PushMessageData",
	"PushSubscriptionChangeEvent", "SyncManager", "SyncEvent", "PeriodicSyncManager", "PeriodicSyncEvent", "Lock", "LockManager",
}

// platformAbsent lists names that must stay absent: hardware and sensor
// APIs whose absence is how pages feature-detect an unsupported device, and
// language features that cannot be provided (Temporal). Names owned by other
// work streams (WebAssembly, IndexedDB, Cache Storage, Service Workers,
// WebRTC) are deliberately not listed either way.
var platformAbsent = []string{
	"navigator.bluetooth", "Bluetooth", "navigator.usb", "USB", "navigator.hid", "HID", "navigator.serial", "Serial",
	"navigator.gpu", "GPU", "GPUAdapter", "GPUDevice", "navigator.xr", "XRSystem", "NDEFReader",
	"Accelerometer", "Gyroscope", "Magnetometer", "AbsoluteOrientationSensor", "AmbientLightSensor",
	"Temporal",
}

func TestPlatformCoverage(t *testing.T) {
	e := newFeatureEngine(t)
	tab, err := e.NewTab("about:blank")
	if err != nil {
		t.Fatal(err)
	}
	probe := `(function (names) {
  function has(path) {
    var o = globalThis, parts = path.split('.');
    for (var i = 0; i < parts.length; i++) {
      if (o === null || o === undefined || !(parts[i] in Object(o))) return false;
      if (i < parts.length - 1) o = o[parts[i]];
    }
    return true;
  }
  function usable(path) {
    var o = globalThis, parts = path.split('.');
    for (var i = 0; i < parts.length; i++) o = o[parts[i]];
    return o !== undefined;
  }
  return names.map(function (n) { var h = has(n); return [n, h, h ? usable(n) : null]; });
})`
	check := func(names []string) (rows []struct {
		Name   string
		Has    bool
		Usable any
	}) {
		js, _ := json.Marshal(names)
		raw := mustEval(t, tab, probe+"("+string(js)+")")
		var res [][]any
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatalf("probe result %s: %v", raw, err)
		}
		for _, r := range res {
			rows = append(rows, struct {
				Name   string
				Has    bool
				Usable any
			}{r[0].(string), r[1].(bool), r[2]})
		}
		return
	}
	var missing, unusable []string
	for _, r := range check(platformImplemented) {
		if !r.Has {
			missing = append(missing, r.Name)
		} else if u, _ := r.Usable.(bool); !u {
			unusable = append(unusable, r.Name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("implemented APIs missing from a fresh tab: %s", strings.Join(missing, ", "))
	}
	if len(unusable) > 0 {
		t.Errorf("implemented APIs present but evaluate to undefined: %s", strings.Join(unusable, ", "))
	}
	var present []string
	for _, r := range check(platformAbsent) {
		if r.Has {
			present = append(present, r.Name)
		}
	}
	if len(present) > 0 {
		t.Errorf("intentionally absent APIs are present (update docs/web-platform.md if this is deliberate): %s", strings.Join(present, ", "))
	}
}
