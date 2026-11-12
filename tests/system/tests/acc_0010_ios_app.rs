//! docs/acceptance/0010-ios-app.md: the iOS app's embedded kcp requester runs in an iOS simulator or device (Xcode on
//! macOS); this Linux harness cannot drive it. Its market path is the Go requester the other tests drive (0004 agent
//! path, 0003 webrtc). Ported when a macOS runner with the simulator joins the pipeline.

#[tokio::test(flavor = "multi_thread")]
async fn a1_a6_need_a_simulator() {
    testkit::run("0010", "A1-A6", &[], |ctx| async move {
        ctx.skip("needs an iOS simulator (macOS runner)")
    })
    .await;
}
