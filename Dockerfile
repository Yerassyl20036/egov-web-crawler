# Use official MongoDB image
FROM mongo:7.0

# Set environment variables
ENV MONGO_INITDB_ROOT_USERNAME=admin
ENV MONGO_INITDB_ROOT_PASSWORD=password
ENV MONGO_INITDB_DATABASE=webCrawlerArchive

# Expose MongoDB port
EXPOSE 27017

# Create initialization script
RUN mkdir -p /docker-entrypoint-initdb.d

# Add initialization script to create the database
RUN echo 'db = db.getSiblingDB("webCrawlerArchive"); \
db.createCollection("webpages"); \
db.createCollection("datasets");' > /docker-entrypoint-initdb.d/init-mongo.js

# Start MongoDB
CMD ["mongod"]
