import CryptoKit
import Foundation
import Security

/// KeyProbe answers, for the user it runs as, which signing keys a daemon can hold:
/// a Secure Enclave key, a software key kept in a file, and a keychain item. It is
/// the B2 test of the signing-identity design: it runs under the role user's
/// LaunchDaemon with no GUI session, and reports each capability as key=value lines.
/// It writes only inside `dir`, and deletes the one keychain item it adds.
public enum KeyProbe {
    /// A fixed message, so two signatures from the same key are comparable runs.
    static let message = Data("pomar key-probe v1".utf8)
    static let keychainService = "org.zeroemployee.pomar.key-probe"

    public struct Result {
        public var lines: [String] = []
        /// True when every step that the platform offers succeeded.
        public var ok = true
        mutating func add(_ key: String, _ value: String) { lines.append("\(key)=\(value)") }
        mutating func fail(_ key: String, _ error: Error) {
            ok = false
            add(key, "error: \(error)")
        }
    }

    public static func run(dir: String) -> Result {
        var r = Result()
        r.add("uid", String(getuid()))
        r.add("user", NSUserName())
        r.add("home", NSHomeDirectory())
        let base = URL(fileURLWithPath: dir, isDirectory: true)
        do {
            try FileManager.default.createDirectory(
                at: base, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        } catch {
            r.fail("dir", error)
            return r
        }
        secureEnclave(base, &r)
        fileKey(base, &r)
        keychain(&r)
        r.add("probe_ok", String(r.ok))
        return r
    }

    /// Approach B: a P-256 key created inside the Secure Enclave. Its private half never
    /// leaves the enclave; dataRepresentation is an opaque blob only this Mac's enclave
    /// can use. The blob is saved at mode 0600, reloaded, and used again.
    static func secureEnclave(_ base: URL, _ r: inout Result) {
        r.add("se_available", String(SecureEnclave.isAvailable))
        guard SecureEnclave.isAvailable else { return }
        do {
            let key = try SecureEnclave.P256.Signing.PrivateKey()
            let sig = try key.signature(for: message)
            r.add("se_sign_verify", String(key.publicKey.isValidSignature(sig, for: message)))
            let blob = base.appendingPathComponent("se-key.blob")
            try write(key.dataRepresentation, to: blob)
            let again = try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: Data(contentsOf: blob))
            let sig2 = try again.signature(for: message)
            r.add("se_reload_verify", String(key.publicKey.isValidSignature(sig2, for: message)))
            r.add("se_public_key_sha256", sha256(key.publicKey.x963Representation))
        } catch {
            r.fail("se", error)
        }
    }

    /// Approach A, as ruled: a software P-256 key in a file at mode 0600, owned by the
    /// role user. Reloaded from the file, it must verify against the original public key.
    static func fileKey(_ base: URL, _ r: inout Result) {
        do {
            let key = P256.Signing.PrivateKey()
            let file = base.appendingPathComponent("file-key.raw")
            try write(key.rawRepresentation, to: file)
            let mode = try FileManager.default.attributesOfItem(atPath: file.path)[.posixPermissions] as? Int
            r.add("file_key_mode", String(format: "%o", mode ?? 0))
            let again = try P256.Signing.PrivateKey(rawRepresentation: Data(contentsOf: file))
            let sig = try again.signature(for: message)
            r.add("file_key_reload_verify", String(key.publicKey.isValidSignature(sig, for: message)))
            r.add("file_public_key_sha256", sha256(key.publicKey.x963Representation))
        } catch {
            r.fail("file_key", error)
        }
    }

    /// Whether the user's default (file-based) keychain can hold an item: add, read back,
    /// delete. Each OSStatus is reported as returned; errSecNoDefaultKeychain (-25307)
    /// means the user has no keychain to use, which is a finding, not a crash.
    static func keychain(_ r: inout Result) {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: keychainService,
            kSecAttrAccount as String: "probe",
        ]
        SecItemDelete(query as CFDictionary)  // a leftover from an earlier run, if any
        var add = query
        add[kSecValueData as String] = Data("probe".utf8)
        let addStatus = SecItemAdd(add as CFDictionary, nil)
        r.add("keychain_add", status(addStatus))
        guard addStatus == errSecSuccess else { return }
        var read = query
        read[kSecReturnData as String] = true
        var out: CFTypeRef?
        let readStatus = SecItemCopyMatching(read as CFDictionary, &out)
        r.add("keychain_read", status(readStatus))
        r.add("keychain_read_matches", String((out as? Data) == Data("probe".utf8)))
        r.add("keychain_delete", status(SecItemDelete(query as CFDictionary)))
    }

    static func write(_ data: Data, to url: URL) throws {
        try? FileManager.default.removeItem(at: url)
        guard FileManager.default.createFile(atPath: url.path, contents: data, attributes: [.posixPermissions: 0o600])
        else { throw CocoaError(.fileWriteUnknown) }
    }

    static func status(_ s: OSStatus) -> String {
        let text = SecCopyErrorMessageString(s, nil) as String? ?? "unknown"
        return "\(s) (\(text))"
    }

    static func sha256(_ d: Data) -> String {
        SHA256.hash(data: d).map { String(format: "%02x", $0) }.joined()
    }
}
