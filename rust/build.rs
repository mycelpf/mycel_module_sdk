fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Compile the canonical SDK schema; no copied protocol lives in the Rust consumer.
    let root = std::path::PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?).join("..");
    let schema = root.join("contracts/modulerpc/v1/modulerpc.proto");
    println!("cargo:rerun-if-changed={}", schema.display());
    tonic_build::configure().compile_protos(&[schema], &[root])?;
    Ok(())
}
