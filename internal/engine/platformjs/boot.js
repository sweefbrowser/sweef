/*
 * Sweef platform layer, bootstrap. Evaluated in every VM, so it is kept small
 * and cheap: a table of which names belong to which module, and one lazy
 * accessor per name. The first touch of a name evaluates platformjs/<module>.js
 * (fetched through __sw.platformSrc), which replaces the accessors with the
 * real properties. Shared helpers live in h.js and load with the first module.
 * See platform.go and docs/web-platform.md.
 */
(function (g) {
  'use strict';
  var sw = g.__sw;
  if (!sw || typeof sw.platformSrc !== 'function') return;
  var loaded = Object.create(null), lazyGets = new Set(), H = null;
  // Defining ~120 accessors on the global object itself is slow (it is a huge object, ~2 ms in
  // every VM); defining them on a small object in its prototype chain is not, and global name
  // lookup, `in`, typeof and assignment all follow the chain. A real definition by a module
  // lands on the global object and shadows the accessor.
  var L = Object.create(Object.getPrototypeOf(g));
  Object.setPrototypeOf(g, L);
  function load(m) {
    if (loaded[m]) return;
    loaded[m] = 1;
    try {
      if (!H) H = (0, g.eval)('(function(g,load,lazyGets){"use strict";' + sw.platformSrc('h') + '\n})')(g, load, lazyGets);
      var src = sw.platformSrc(m);
      if (!src) throw new Error('platform module missing: ' + m);
      (0, g.eval)('(function(g,H){"use strict";' + src + '\n})')(g, H);
    } catch (e) { loaded[m] = 0; throw e; }
  }
  // One record per name, captured by both accessors, keeps closure creation cheap.
  function resolve(r, self) {
    load(r.m);
    var o = r.o === L ? g : r.o, d = Object.getOwnPropertyDescriptor(o, r.n);
    if (!d || lazyGets.has(d.get)) throw new Error('platform: ' + r.n + ' was not defined by module ' + r.m);
    return Reflect.get(o, r.n, self);
  }
  function assign(r, self, v) {
    Object.defineProperty(r.o === L ? g : self, r.n, { value: v, writable: true, configurable: true, enumerable: r.e });
  }
  var desc = { configurable: true, enumerable: false, get: null, set: null };
  function lazy(r) {
    var get = function () { return resolve(r, this); };
    lazyGets.add(get);
    desc.get = get; desc.set = function (v) { assign(r, this, v); }; desc.enumerable = r.e;
    Object.defineProperty(r.o, r.n, desc);
  }
  // [module, target, names, enumerable, only-when-missing]
  var D = g.Document.prototype, N = g.Navigator.prototype, T = [
    ['streams', L, 'ReadableStream ReadableStreamDefaultReader ReadableStreamBYOBReader ReadableStreamDefaultController ReadableByteStreamController ReadableStreamBYOBRequest WritableStream WritableStreamDefaultWriter WritableStreamDefaultController TransformStream TransformStreamDefaultController CountQueuingStrategy ByteLengthQueuingStrategy'],
    ['codecs', L, 'CompressionStream DecompressionStream TextEncoderStream TextDecoderStream'],
    ['xpath', L, 'XPathEvaluator XPathExpression XPathResult XPathNSResolver'],
    ['xpath', D, 'evaluate createExpression createNSResolver', 1],
    ['urlpattern', L, 'URLPattern'],
    ['nav', L, 'navigation Navigation NavigationHistoryEntry NavigateEvent NavigationDestination NavigationTransition NavigationActivation NavigationCurrentEntryChangeEvent'],
    ['vt', L, 'ViewTransition ViewTransitionTypeSet'],
    ['vt', D, 'startViewTransition activeViewTransition', 1],
    ['internals', L, 'ElementInternals CustomStateSet'],
    ['internals', L, 'ValidityState', 0, 1],
    ['internals', g.Element.prototype, 'attachInternals', 1],
    ['css', L, 'AbstractRange Highlight HighlightRegistry CSSPropertyRule ContentVisibilityAutoStateChangeEvent'],
    ['css', L, 'CSSRule', 0, 1],
    ['css', g.CSS, 'highlights registerProperty registeredProperty', 1],
    ['devices', L, 'ScreenOrientation DeviceOrientationEvent DeviceMotionEvent DeviceMotionEventAcceleration DeviceMotionEventRotationRate Gamepad GamepadButton GamepadEvent NavigatorUAData WakeLockSentinel WakeLock MediaMetadata MediaSession Credential PasswordCredential FederatedCredential PublicKeyCredential CredentialsContainer PaymentRequest PaymentResponse PaymentRequestUpdateEvent TrustedHTML TrustedScript TrustedScriptURL TrustedTypePolicy TrustedTypePolicyFactory trustedTypes PictureInPictureWindow PictureInPictureEvent'],
    ['devices', N, 'getGamepads wakeLock mediaSession credentials userAgentData', 1],
    ['devices', g.Screen.prototype, 'orientation', 1],
    ['devices', D, 'hasStorageAccess requestStorageAccess', 1],
    ['devices', D, 'pictureInPictureEnabled pictureInPictureElement exitPictureInPicture', 1, 1], // the media prelude has the Chromium-matched versions
    ['opfs', L, 'FileSystemHandle FileSystemFileHandle FileSystemDirectoryHandle FileSystemWritableFileStream showOpenFilePicker showSaveFilePicker showDirectoryPicker'],
    ['es', Array, 'fromAsync', 0, 1],
    ['es', Symbol, 'dispose asyncDispose', 0, 1],
    ['es', L, 'DisposableStack AsyncDisposableStack SuppressedError', 0, 1],
    ['es', Error, 'captureStackTrace', 0, 1],
    ['intl', g.Intl, 'Locale', 0, 1],
    ['intlnum', g.Intl, 'NumberFormat'],
    ['intldate', g.Intl, 'DateTimeFormat'],
    ['intlmisc', g.Intl, 'PluralRules RelativeTimeFormat ListFormat DisplayNames DurationFormat Collator Segmenter getCanonicalLocales supportedValuesOf'],
    ['sched', L, 'scheduler Scheduler TaskController TaskSignal TaskPriorityChangeEvent'],
    ['pwa', L, 'Notification NotificationEvent Permissions PermissionStatus PushManager PushSubscription PushSubscriptionOptions PushEvent PushMessageData PushSubscriptionChangeEvent Clipboard ClipboardItem SyncManager SyncEvent PeriodicSyncManager PeriodicSyncEvent Lock LockManager __pwa __pwaEvent __pwaDispatch'],
    ['pwa', N, 'permissions locks setAppBadge clearAppBadge clipboard', 1],
    ['crypto', L, 'CryptoKey SubtleCrypto'],
    ['crypto', g.Crypto.prototype, 'subtle', 1]
  ];
  if (g.HTMLVideoElement) T.push(['devices', g.HTMLVideoElement.prototype, 'requestPictureInPicture disablePictureInPicture', 1, 1]);
  for (var i = 0; i < T.length; i++) {
    var row = T[i], ns = row[2].split(' ');
    if (!row[1]) continue;
    for (var k = 0; k < ns.length; k++) {
      if (row[4] && ns[k] in row[1]) continue;
      lazy({ m: row[0], o: row[1], n: ns[k], e: !!row[3] });
    }
  }
})(globalThis);
