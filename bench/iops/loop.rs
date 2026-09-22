// #204 floor: rustc -O while black_box(a) < black_box(n) { a = black_box(a)+1 }
use std::hint::black_box;
use std::time::Instant;

fn main() {
    let n: i64 = std::env::var("IOPS_N")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(1_000_000);
    let mut a: i64 = 0;
    let t0 = Instant::now();
    while black_box(a) < black_box(n) {
        a = black_box(a) + 1;
    }
    let ns = t0.elapsed().as_nanos();
    if a != n {
        eprintln!("rust: a={a} want {n}");
        std::process::exit(1);
    }
    println!(
        "rust n={n} ns={ns} ns/iter={:.3} a={a}",
        ns as f64 / n as f64
    );
}
