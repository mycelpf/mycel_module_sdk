//! Canonical Rust module bindings and the app_core Unix-socket runtime.
//! IAM is verified by the host; this library only enforces the transported decision.
pub mod rpc {
    tonic::include_proto!("mycel.modulerpc.v1");
}

use prost::Message;
use std::{
    os::unix::fs::{DirBuilderExt, FileTypeExt, MetadataExt, PermissionsExt},
    path::Path,
};
use tokio_stream::{wrappers::UnixListenerStream, StreamExt};
pub use tonic;

pub const MAX_MESSAGE_BYTES: usize = 32 << 20;
pub const MAX_BODY_BYTES: usize = 30 << 20;
pub const MAX_HEADER_BYTES: usize = 64 << 10;

/// Trusted module deployment binding. Never construct this from HTTP headers.
#[derive(Clone, Debug)]
pub struct AccessBinding {
    pub issuer: String,
    pub account: String,
    pub application: String,
    pub audience: String,
}

/// Match exact IAM grants, without role mapping or module-local validity grace.
pub fn authorized(
    user: Option<&rpc::UserContext>,
    binding: &AccessBinding,
    permission: &str,
    resource: &str,
    now: i64,
) -> bool {
    let Some(user) = user else {
        return false;
    };
    let Some(a) = user.access.as_ref() else {
        return false;
    };
    a.version == 1
        && !user.sub.is_empty()
        && a.subject == user.sub
        && !binding.issuer.is_empty()
        && a.issuer == binding.issuer
        && !binding.account.is_empty()
        && a.account == binding.account
        && !binding.application.is_empty()
        && a.application == binding.application
        && !binding.audience.is_empty()
        && a.audience == binding.audience
        && (user.tenant.is_empty() || user.tenant == a.account)
        && !a.policy_version.trim().is_empty()
        && a.not_before >= 0
        && a.expires_at > a.not_before
        && now >= a.not_before
        && now < a.expires_at
        && !permission.trim().is_empty()
        && !resource.trim().is_empty()
        && a.grants
            .iter()
            .any(|g| g.permission == permission && g.resource == resource)
}

pub fn check_request(request: &rpc::HandleRequest) -> Result<(), tonic::Status> {
    let header_bytes: usize = if request.header_fields.is_empty() {
        request.headers.iter().map(|(k, v)| k.len() + v.len()).sum()
    } else {
        request
            .header_fields
            .iter()
            .map(|h| h.name.len() + h.values.iter().map(String::len).sum::<usize>())
            .sum()
    };
    if request.body.len() > MAX_BODY_BYTES
        || header_bytes > MAX_HEADER_BYTES
        || request.encoded_len() > MAX_MESSAGE_BYTES
    {
        return Err(tonic::Status::resource_exhausted(
            "module request budget exceeded",
        ));
    }
    Ok(())
}

/// Serve only the supervisor-provided Unix socket, accepting same-UID peers.
/// Existing files are never removed to make a bind succeed.
pub async fn serve<T, F>(
    module: T,
    listen: &str,
    shutdown: F,
) -> Result<(), Box<dyn std::error::Error>>
where
    T: rpc::module_handler_server::ModuleHandler,
    F: std::future::Future<Output = ()> + Send + 'static,
{
    let path = listen
        .strip_prefix("unix://")
        .filter(|p| !p.is_empty())
        .ok_or("MYCEL_LISTEN must be a unix:// socket")?;
    let path = Path::new(path);
    if !path.is_absolute() {
        return Err("module socket must be absolute".into());
    }
    let parent = path.parent().ok_or("module socket has no parent")?;
    // SAFETY: geteuid takes no pointers and cannot fail.
    let uid = unsafe { libc::geteuid() };
    // The supervisor supplies the address; each SDK creates the shared socket
    // directory on first boot. Create atomically with private permissions and
    // still reject existing public directories, symlinks and another UID.
    match std::fs::DirBuilder::new().mode(0o700).create(parent) {
        Ok(()) => {},
        Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {},
        Err(e) => return Err(e.into()),
    }
    let meta = std::fs::symlink_metadata(parent)?;
    if !meta.is_dir() || meta.uid() != uid || meta.permissions().mode() & 0o077 != 0 {
        return Err("module socket requires a private same-UID directory".into());
    }
    if let Ok(meta) = std::fs::symlink_metadata(path) {
        if !meta.file_type().is_socket() || meta.uid() != uid {
            return Err("existing module socket path is not an owned socket".into());
        }
        match tokio::net::UnixStream::connect(path).await {
            Ok(_) => return Err("module socket is already serving".into()),
            Err(e) if e.kind() == std::io::ErrorKind::ConnectionRefused => {
                std::fs::remove_file(path)?
            }
            Err(e) => return Err(e.into()),
        }
    }
    let listener = tokio::net::UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    let incoming = UnixListenerStream::new(listener).filter_map(move |item| match item {
        Ok(stream) if stream.peer_cred().map(|p| p.uid() == uid).unwrap_or(false) => {
            Some(Ok::<_, std::io::Error>(stream))
        }
        _ => None,
    });
    let module = std::sync::Arc::new(module);
    let (mut reporter, health_service) = tonic_health::server::health_reporter();
    // app_core's supervisor uses grpc.health.v1, independently of the module RPC.
    // Keep its readiness tied to the module's actual health, including recovery.
    let health_module = module.clone();
    let health_monitor = async move {
        loop {
            let status = match tokio::time::timeout(
                std::time::Duration::from_secs(1),
                health_module.health(tonic::Request::new(rpc::HealthRequest {})),
            ).await {
                Ok(Ok(response)) if response.get_ref().status == 1 => tonic_health::ServingStatus::Serving,
                _ => tonic_health::ServingStatus::NotServing,
            };
            reporter.set_service_status("", status).await;
            reporter.set_service_status("mycel.modulerpc.v1.ModuleHandler", status).await;
            tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        }
    };
    let service = rpc::module_handler_server::ModuleHandlerServer::from_arc(module)
        .max_decoding_message_size(MAX_MESSAGE_BYTES)
        .max_encoding_message_size(MAX_MESSAGE_BYTES);
    let (stopping, stopped) = tokio::sync::oneshot::channel();
    let shutdown = async move {
        shutdown.await;
        let _ = stopping.send(());
    };
    let server = tonic::transport::Server::builder()
        .add_service(health_service)
        .add_service(service)
        .serve_with_incoming_shutdown(incoming, shutdown);
    tokio::pin!(server);
    tokio::pin!(health_monitor);
    let result = tokio::select! {
        _=&mut health_monitor=>Err("module health monitor stopped".into()),
        result=&mut server=>result.map_err(|e|e.to_string()),
        _=stopped=>match tokio::time::timeout(std::time::Duration::from_secs(15),&mut server).await {
            Ok(result)=>result.map_err(|e|e.to_string()),
            Err(_)=>Err("module shutdown budget exceeded".into()),
        }
    };
    let _ = std::fs::remove_file(path);
    result?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn fixture() -> (rpc::UserContext, AccessBinding) {
        let b = AccessBinding {
            issuer: "https://iam.example".into(),
            account: "a".into(),
            application: "helix".into(),
            audience: "client".into(),
        };
        let user = rpc::UserContext {
            sub: "p".into(),
            tenant: "a".into(),
            access: Some(rpc::AuthorizedContext {
                version: 1,
                issuer: b.issuer.clone(),
                subject: "p".into(),
                account: "a".into(),
                application: "helix".into(),
                audience: "client".into(),
                policy_version: "7".into(),
                not_before: 100,
                expires_at: 200,
                grants: vec![rpc::PermissionGrant {
                    permission: "helix:read".into(),
                    resource: "kb:one".into(),
                }],
            }),
            ..Default::default()
        };
        (user, b)
    }
    #[test]
    fn exact_iam_grant_and_time_binding() {
        let (user, b) = fixture();
        assert!(authorized(Some(&user), &b, "helix:read", "kb:one", 150));
        for time in [99, 200, 201] {
            assert!(!authorized(Some(&user), &b, "helix:read", "kb:one", time));
        }
        assert!(!authorized(Some(&user), &b, "helix:write", "kb:one", 150));
        assert!(!authorized(Some(&user), &b, "helix:read", "kb:other", 150));
        let encoded = user.encode_to_vec();
        assert_eq!(rpc::UserContext::decode(encoded.as_slice()).unwrap(), user);
    }
    #[test]
    fn roles_cannot_grant_access_and_bindings_cannot_be_substituted() {
        let (mut user, b) = fixture();
        user.access.as_mut().unwrap().subject = "other".into();
        assert!(!authorized(Some(&user), &b, "helix:read", "kb:one", 150));
        user.access = None;
        user.roles = vec!["admin".into()];
        assert!(!authorized(Some(&user), &b, "helix:read", "kb:one", 150));
    }
}
