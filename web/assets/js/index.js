const slides = Array.from(document.querySelectorAll(".carousel-slide"));
const dots = Array.from(document.querySelectorAll(".carousel-dot"));
const prevButton = document.getElementById("carousel-prev");
const nextButton = document.getElementById("carousel-next");
const intervalMs = 3500;

let currentIndex = 0;
let timerId = null;

function renderCarousel(index) {
  currentIndex = (index + slides.length) % slides.length;

  slides.forEach((slide, slideIndex) => {
    slide.classList.toggle("is-active", slideIndex === currentIndex);
  });

  dots.forEach((dot, dotIndex) => {
    dot.classList.toggle("is-active", dotIndex === currentIndex);
  });
}

function moveCarousel(step) {
  renderCarousel(currentIndex + step);
}

function stopAutoPlay() {
  if (timerId !== null) {
    window.clearInterval(timerId);
    timerId = null;
  }
}

function startAutoPlay() {
  stopAutoPlay();
  timerId = window.setInterval(() => {
    moveCarousel(1);
  }, intervalMs);
}

prevButton?.addEventListener("click", () => {
  moveCarousel(-1);
  startAutoPlay();
});

nextButton?.addEventListener("click", () => {
  moveCarousel(1);
  startAutoPlay();
});

dots.forEach((dot, index) => {
  dot.addEventListener("click", () => {
    renderCarousel(index);
    startAutoPlay();
  });
});

if (slides.length > 0) {
  renderCarousel(0);
  startAutoPlay();
}
